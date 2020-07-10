package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/ChainSafe/gossamer/lib/common"
)

var keys = []string{"alice", "bob", "charlie", "dave", "eve", "ferdie", "george", "heather", "ian"}
var genesisThreeAuths = "genesis_threeauths.json"
var genesisSixAuths = "genesis_sixauths.json"
var genesisNineAuths = "genesis.json"
var config = "config.toml"

var (
	maxRetries        = 10
	httpClientTimeout = 120 * time.Second
	dialTimeout       = 60 * time.Second

	transport = &http.Transport{
		Dial: (&net.Dialer{
			Timeout: dialTimeout,
		}).Dial,
	}
	httpClient = &http.Client{
		Transport: transport,
		Timeout:   httpClientTimeout,
	}
)

// ServerResponse wraps the RPC response
type ServerResponse struct {
	// JSON-RPC Version
	Version string `json:"jsonrpc"`
	// Resulting values
	Result json.RawMessage `json:"result"`
	// Any generated errors
	Error *Error `json:"error"`
	// Request id
	ID *json.RawMessage `json:"id"`
}

// ErrCode is a int type used for the rpc error codes
type ErrCode int

// Error is a struct that holds the error message and the error code for a error
type Error struct {
	Message   string                 `json:"message"`
	ErrorCode ErrCode                `json:"code"`
	Data      map[string]interface{} `json:"data"`
}

func postRPC(method, host, params string) ([]byte, error) {
	data := []byte(`{"jsonrpc":"2.0","method":"` + method + `","params":` + params + `,"id":1}`)
	buf := &bytes.Buffer{}
	_, err := buf.Write(data)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	r, err := http.NewRequest("POST", host, buf)
	if err != nil {
		return nil, err
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(r)
	if err != nil {
		return nil, err
	} else if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code not OK")
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	return respBody, nil
}

func decodeRPC(body []byte, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var response ServerResponse
	err := decoder.Decode(&response)
	if err != nil {
		return err
	}

	if response.Error != nil {
		return errors.New(response.Error.Message)
	}

	decoder = json.NewDecoder(bytes.NewReader(response.Result))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func getFinalizedHeadByRound(endpoint string, round uint64) (common.Hash, error) {
	p := strconv.Itoa(int(round))
	respBody, err := postRPC("chain_getFinalizedHeadByRound", endpoint, "["+p+"]")
	if err != nil {
		return common.Hash{}, err
	}

	var hash string
	err = decodeRPC(respBody, &hash)
	if err != nil {
		return common.Hash{}, err
	}

	return common.MustHexToHash(hash), nil
}

func initAndStart(idx int, genesis string, outfile *os.File) *exec.Cmd {
	basepath := "~/.gossamer_" + keys[idx]

	initCmd := exec.Command("../../ChainSafe/gossamer/bin/gossamer",
		"init",
		"--config", config,
		"--basepath", basepath,
		"--genesis", genesis,
		"--force",
	)

	// init gossamer
	stdout, err := initCmd.CombinedOutput()
	if err != nil {
		panic(err)
	}

	outfile.Write(stdout)
	fmt.Println("initialized node", keys[idx])

	gssmrCmd := exec.Command("../../ChainSafe/gossamer/bin/gossamer",
		"--port", strconv.Itoa(7000+idx),
		"--config", config,
		"--key", keys[idx],
		"--basepath", basepath,
		"--rpcport", strconv.Itoa(8540+idx),
		"--rpc",
		"--log", "debug",
	)

	stdoutPipe, err := gssmrCmd.StdoutPipe()
	if err != nil {
		panic(err)
	}

	err = gssmrCmd.Start()
	if err != nil {
		panic(err)
	}

	writer := bufio.NewWriter(outfile)
	go io.Copy(writer, stdoutPipe)
	return gssmrCmd
}

func main() {
	baseport := 8540
	num := 3
	var err error

	if len(os.Args) > 1 {
		num, err = strconv.Atoi(os.Args[1])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		if num%3 != 0 {
			fmt.Print("must do 3, 6, 9 nodes")
			os.Exit(1)
		}
	}

	fmt.Println("num nodes:", num)

	var genesis string
	switch num {
	case 3:
		genesis = genesisThreeAuths
	case 6:
		genesis = genesisSixAuths
	case 9:
		genesis = genesisNineAuths
	}

	// initialize and start nodes
	processes := []*exec.Cmd{}

	var wg sync.WaitGroup
	wg.Add(num)
	for i := 0; i < num; i++ {
		outfile, err := os.Create("./log_" + keys[i] + ".out")
		if err != nil {
			panic(err)
		}
		defer outfile.Close()

		go func(i int, outfile *os.File) {
			p := initAndStart(i, genesis, outfile)
			processes = append(processes, p)
			wg.Done()
		}(i, outfile)
	}
	wg.Wait()

	for i := 0; i < num; i++ {
		go func(i int) {
			err = processes[i].Wait()
			if err != nil {
				fmt.Printf("process %s failed!!! %s\n", keys[i], err)
			}
		}(i)
	}

	// wait for node to start
	time.Sleep(time.Second * 5)

	rounds := 20
	empty := [32]byte{}

	// queqy for finalized block in each round
	for i := 1; i < rounds; i++ {
		var wg sync.WaitGroup
		wg.Add(num)

		fmt.Println("getting finalized block for round", i)

		for j := 0; j < num; j++ {

			go func(round, node int) {
				var res common.Hash
				for {
					res, err = getFinalizedHeadByRound("http://localhost:"+strconv.Itoa(baseport+node), uint64(round))
					if err == nil && !bytes.Equal(res[:], empty[:]) {
						break
					}

					time.Sleep(time.Second)
				}

				fmt.Printf("round %d: got finalized hash from node %d: %s\n", round, node, res)
				wg.Done()
			}(i, j)

		}

		wg.Wait()
		time.Sleep(time.Second * 3)
	}
}
