wsrpc
=====

Module `github.com/jrick/wsrpc` provides a partial implementation of a JSON-RPC
2.0 websocket client.  Inspired by net/rpc, clients call methods by their name
with arguments and return values marshaled by encoding/json.  The client may be
used to create convenience calls with types specific to an application.

Receiving notifications is supported but it is up to the caller to unmarshal the
JSON-RPC parameters into meaningful data.

This module currently does not implement JSON-RPC 2.0 request batching or keyed
request parameters when performing calls.

## CLI

A command line tool is provided to perform individual websocket JSON-RPCs
against a server.

A JSON array must be used to passed parameters to a RPC.

```
$ wsrpc -h
usage: wsrpc address [flags] method [arg]
  -c string
        Root certificate PEM file
  -p string
        Password
  -u string
        User
$ wsrpc wss://dcrd0.i.zettaport.com:9109/ws -c dcrd0.pem -u jrick -p sekrit getinfo
{
  "version": 1050000,
  "protocolversion": 6,
  "blocks": 324795,
  "timeoffset": 0,
  "connections": 65,
  "proxy": "",
  "difficulty": 19920803496.64989,
  "testnet": false,
  "relayfee": 0.0001,
  "errors": ""
}
$ wsrpc wss://dcrd0.i.zettaport.com:9109/ws -c dcrd0.pem -u jrick -p sekrit getblockhash '[324795]'
"0000000000000000235b1210221d412c428237175dbb0aef202277d1706b9312"
```

## License

wsrpc is implemented under the liberal ISC License.
