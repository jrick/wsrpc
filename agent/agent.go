// Package agent provides programmatic access to a running wsrpc-agent process.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
)

const (
	sockEnv = "WSRPCAGENT_SOCK"
	authEnv = "WSRPCAGENT_AUTH"
)

// EnvironmentSet returns whether the environment variables have been set to
// communicate with the agent process.
func EnvironmentSet() bool {
	sock, auth := os.Getenv(sockEnv), os.Getenv(authEnv)
	if sock == "" || auth == "" {
		return false
	}
	_, err := os.Stat(sock)
	return !os.IsNotExist(err)
}

// Dial opens a connection to the unix socket of the agent process.
func Dial() (net.Conn, error) {
	sock := os.Getenv(sockEnv)
	return net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
}

// Args is the message, encoded to JSON, which is written to the agent socket to
// initiate an RPC.
type Args struct {
	Address  string
	RootCert string
	User     string
	Pass     string
	Method   string
	Params   string
}

// RPC dials the agent and performs the RPC described by args.  The JSON-RPC
// result is returned as a raw message.
func RPC(args *Args) (json.RawMessage, error) {
	agent, err := Dial()
	if err != nil {
		return nil, err
	}
	defer agent.Close()
	enc := json.NewEncoder(agent)
	auth := os.Getenv(authEnv)
	if err := enc.Encode(auth); err != nil {
		return nil, err
	}
	if err := enc.Encode(args); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(agent)
	var es string
	if err := dec.Decode(&es); err != nil {
		return nil, err
	}
	if es != "" {
		return nil, errors.New(es)
	}
	var res json.RawMessage
	err = dec.Decode(&res)
	return res, err
}

// Client holds the connection information required to provide the agent process
// to connect to a particular JSON-RPC websocket server.
type Client struct {
	Address  string
	RootCert string
	User     string
	Pass     string
}

// Call opens an agent connection to perform a JSON-RPC call.  If a connection
// to the address is not already established, the agent process opens a
// connection and persists this for future calls.
func (c *Client) Call(_ context.Context, method string, result interface{}, args ...interface{}) error {
	rawParams, err := json.Marshal(args)
	if err != nil {
		return err
	}
	agargs := &Args{
		Address:  c.Address,
		RootCert: c.RootCert,
		User:     c.User,
		Pass:     c.Pass,
		Method:   method,
		Params:   string(rawParams),
	}
	rawResult, err := RPC(agargs)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(rawResult), result)
}
