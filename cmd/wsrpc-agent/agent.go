package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/jrick/wsrpc/v2"
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

const (
	sockEnv = "WSRPCAGENT_SOCK"
	authEnv = "WSRPCAGENT_AUTH"
	pidEnv  = "WSRPCAGENT_PID"
)

func main() {
	// Go has no fork/exec (already multithreaded before main), so spawning
	// ourselves with undocumented -daemon=ppid flag is next best thing.
	if len(os.Args) >= 2 && strings.HasPrefix(os.Args[1], "-daemon=") {
		if strconv.Itoa(os.Getppid()) == os.Args[1][len("-daemon="):] {
			daemon()
			return
		}
		log.Fatal("bad ppid")
	}

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage (exec):\twsrpc-agent cmd [args...]")
		fmt.Fprintln(os.Stderr, "usage (daemon):\teval $(wsrpc-agent)")
		os.Exit(2)
	}
	flag.Parse()

	if os.Getenv(sockEnv) != "" {
		log.Fatal(sockEnv + " is set")
	}
	if os.Getenv(authEnv) != "" {
		log.Fatal(authEnv + " is set")
	}

	args := make([]string, 1, 2)
	args[0] = fmt.Sprintf("-daemon=%d", os.Getpid())
	if len(os.Args) > 1 {
		args = append(args, "-exec")
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Stderr = os.Stderr
	daemonStdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
	daemonStdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	s := bufio.NewScanner(daemonStdout)
	scanText := func() string {
		s.Scan()
		return s.Text()
	}
	auth := scanText()
	sock := scanText()
	cpid := scanText()
	if err := s.Err(); err != nil {
		log.Fatal(err)
	}

	if len(os.Args) == 1 {
		fmt.Printf(authEnv+"='%s'; export "+authEnv+";\n", auth)
		fmt.Printf(sockEnv+"='%s'; export "+sockEnv+";\n", sock)
		fmt.Printf(pidEnv+"='%s'; export "+pidEnv+";\n", cpid)
		fmt.Printf("echo 'Agent listening on %v'\n", sock)
		return
	}

	setenv := func(key, value string) {
		if err := os.Setenv(key, value); err != nil {
			log.Fatal(err)
		}
	}
	setenv(authEnv, auth)
	setenv(sockEnv, sock)
	setenv(pidEnv, cpid)
	cmd = exec.Command(os.Args[1], os.Args[2:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	daemonStdin.Close()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		log.Fatal(err)
	}
}

func daemon() {
	err := pledge("stdio rpath wpath cpath inet fattr unix dns unveil")
	if err != nil {
		log.Fatalf("pledge: %v", err)
	}

	log.SetPrefix("wsrpc-agent: ")

	err = unveil("/tmp", "rwc")
	if err != nil {
		log.Fatalf("unveil: %v", err)
	}

	tmpDir, err := os.MkdirTemp("", "wsrpc")
	if err != nil {
		log.Fatal(err)
	}
	sockName := filepath.Join(tmpDir, fmt.Sprintf("agent.%d", os.Getpid()))
	lis, err := net.Listen("unix", sockName)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(sockName, 0o600); err != nil {
		log.Fatal(err)
	}

	err = unveil(tmpDir, "c")
	if err != nil {
		log.Fatalf("unveil: %v", err)
	}
	err = unveil("/tmp", "")
	if err != nil {
		log.Fatalf("unveil: %v", err)
	}
	err = unveil("/etc/ssl", "r")
	if err != nil {
		log.Fatalf("unveil: %v", err)
	}
	err = unveilBlock()
	if err != nil {
		log.Fatalf("unveil: %v", err)
	}
	err = pledge("stdio rpath cpath inet dns")
	if err != nil {
		log.Fatalf("pledge: %v", err)
	}

	authBytes := make([]byte, 32)
	_, err = rand.Read(authBytes)
	if err != nil {
		log.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString(authBytes)

	// Write auth, socket, and pid to parent.
	fmt.Println(auth)
	fmt.Println(lis.Addr())
	fmt.Println(os.Getpid())
	os.Stdout.Close()

	// Exit when parent closes stdin if parent is executing command.
	if len(os.Args) == 3 && os.Args[2] == "-exec" {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			os.RemoveAll(tmpDir)
			os.Exit(1)
		}()
	}

	ag := agent{
		auth:    auth,
		lis:     lis,
		clients: make(map[string]*wsrpc.Client),
	}
	ag.listen()
}

type agent struct {
	auth    string
	lis     net.Listener
	clients map[string]*wsrpc.Client
	mu      sync.Mutex
}

func (ag *agent) listen() {
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
	if auth != ag.auth {
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
	c, ok := ag.clients[addr]
	if !ok {
		c, err = dial(addr, args.RootCert, args.User, args.Pass)
		if err != nil {
			err = fmt.Errorf("dial: %v", err)
		} else {
			go func() {
				c.Err()
				ag.mu.Lock()
				delete(ag.clients, addr)
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
