// Copyright (c) 2019 Josh Rickmar
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/jrick/wsrpc"
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

const sockEnv = "WSRPCAGENT_SOCK"
const authEnv = "WSRPCAGENT_AUTH"
const pidEnv = "WSRPCAGENT_PID"

func main() {
	// Go has no fork/exec (already multithreaded before main), so spawning
	// ourselves with undocumented -daemon=ppid flag is next best thing.
	if len(os.Args) == 2 && strings.HasPrefix(os.Args[1], "-daemon=") {
		if strconv.Itoa(os.Getppid()) == os.Args[1][len("-daemon="):] {
			daemon()
			return
		}
		log.Fatal("bad ppid")
	}

	if runtime.GOOS == "linux" {
		var username string
		if u, err := user.Current(); err == nil {
			username = u.Username
		}
		log.Printf("Warning: Linux exposes environment variables via /proc "+
			"and "+authEnv+" is readable by user %v", username)
	}

	if os.Getenv(sockEnv) != "" {
		log.Fatal(sockEnv + " is set")
	}
	if os.Getenv(authEnv) != "" {
		log.Fatal(authEnv + " is set")
	}

	cmd := exec.Command(os.Args[0], fmt.Sprintf("-daemon=%d", os.Getpid()))
	cmd.Stderr = os.Stderr
	daemonStdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	if _, err := io.Copy(os.Stdout, daemonStdout); err != nil && err != io.EOF {
		log.Fatal(err)
	}
}

func daemon() {
	log.SetPrefix("wsrpc-agent: ")

	authBytes := make([]byte, 32)
	_, err := rand.Read(authBytes)
	if err != nil {
		log.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString(authBytes)

	tmpDir, err := ioutil.TempDir("", "wsrpc")
	if err != nil {
		log.Fatal(err)
	}
	sockName := filepath.Join(tmpDir, fmt.Sprintf("agent.%d", os.Getpid()))
	lis, err := net.Listen("unix", sockName)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(sockName, 0600); err != nil {
		log.Fatal(err)
	}

	// Write output for, and close, parent process.
	fmt.Printf(authEnv+"='%s'; export "+authEnv+";\n", auth)
	fmt.Printf(sockEnv+"='%s'; export "+sockEnv+";\n", lis.Addr())
	fmt.Printf(pidEnv+"='%d'; export "+pidEnv+";\n", os.Getpid())
	fmt.Printf("echo 'Agent listening on %v'\n", lis.Addr())
	os.Stdout.Close()

	ag := agent{
		tmp:     tmpDir,
		auth:    auth,
		lis:     lis,
		clients: make(map[string]*wsrpc.Client),
	}
	ag.listen()
}

type agent struct {
	tmp     string
	auth    string
	lis     net.Listener
	clients map[string]*wsrpc.Client
	mu      sync.Mutex
}

func (ag *agent) listen() {
	defer os.RemoveAll(ag.tmp)

	for {
		conn, err := ag.lis.Accept()
		if err != nil {
			return
		}
		go ag.serve(conn)
	}
}

func (ag *agent) serve(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	var auth string
	if err := dec.Decode(&auth); err != nil {
		log.Println(err)
		return
	}
	if string(auth) != ag.auth {
		return
	}
	var args struct {
		Address  string
		RootCert string
		User     string
		Pass     string
		Method   string
		Params   string
	}
	if err := dec.Decode(&args); err != nil {
		log.Println(err)
		return
	}
	addr := args.Address
	var res json.RawMessage
	var err error
	ag.mu.Lock()
	c, ok := ag.clients[args.Address]
	if !ok {
		c, err = dial(addr, args.RootCert, args.User, args.Pass)
		if err != nil {
			err = fmt.Errorf("dial: %v", err)
		} else {
			go func() {
				c.Err()
				ag.mu.Lock()
				delete(ag.clients[addr])
				ag.mu.Unlock()
			}()
			ag.clients[addr] = c
		}
	}
	ag.mu.Unlock()
	if err == nil {
		var a []interface{}
		if args.Params != "" {
			err = json.NewDecoder(strings.NewReader(args.Params)).Decode(&a)
		}
		if err == nil {
			err = c.Call(context.Background(), args.Method, &res, a...)
		}
	}
	var es string
	if err != nil {
		es = err.Error()
	}
	if err := enc.Encode(es); err != nil {
		log.Println(err)
		return
	}
	if err := enc.Encode(res); err != nil {
		log.Println(err)
	}
}

func dial(addr, rootCert, user, pass string) (*wsrpc.Client, error) {
	var tc *tls.Config
	if rootCert != "" {
		tc = &tls.Config{RootCAs: x509.NewCertPool()}
		if !tc.RootCAs.AppendCertsFromPEM([]byte(rootCert)) {
			return nil, errors.New("unparsable root certificate chain")
		}
	}
	return wsrpc.Dial(context.Background(), addr, wsrpc.WithTLSConfig(tc), wsrpc.WithBasicAuth(user, pass))
}
