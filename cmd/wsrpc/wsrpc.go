// Copyright (c) 2019 Josh Rickmar
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"github.com/jrick/wsrpc"
)

var (
	fs       = flag.NewFlagSet("", flag.ExitOnError)
	cFlag    = fs.String("c", "", "Root certificate chain")
	userFlag = fs.String("u", "", "User")
	passFlag = fs.String("p", "", "Password")
)

func main() {
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: wsrpc address [flags] method [arg]")
		fs.PrintDefaults()
		os.Exit(2)
	}
	if len(os.Args) < 2 {
		fs.Usage()
	}
	addr := os.Args[1]
	fs.Parse(os.Args[2:])
	n := fs.NArg()
	if n != 1 && n != 2 { // Expect method and optionally a JSON array arg
		fs.Usage()
	}
	method, arg := fs.Arg(0), ""
	if n == 2 {
		arg = fs.Arg(1)
		if arg != "" && arg[0] != '[' {
			log.Fatal("parameter must be JSON array")
		}
	}
	ctx := context.Background()
	var tc *tls.Config
	if *cFlag != "" {
		pem, err := ioutil.ReadFile(*cFlag)
		if err != nil {
			log.Fatal(err)
		}
		tc = &tls.Config{RootCAs: x509.NewCertPool()}
		if !tc.RootCAs.AppendCertsFromPEM(pem) {
			log.Fatal("unparsable root certificate chain")
		}
	}
	c, err := wsrpc.Dial(ctx, addr, wsrpc.WithTLSConfig(tc), wsrpc.WithBasicAuth(*userFlag, *passFlag))
	if err != nil {
		log.Fatal(err)
	}
	if err := run(ctx, c, method, arg); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, c *wsrpc.Client, method string, arg string) error {
	defer c.Close()
	var args []interface{}
	if arg != "" {

		if err := json.NewDecoder(strings.NewReader(arg)).Decode(&args); err != nil {
			return err
		}
	}
	var res json.RawMessage
	if err := c.Call(ctx, method, &res, args...); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
