package main

import (
	"github.com/eberle1080/mcp/bridge/mcp"
	_ "github.com/viant/scy/kms/blowfish"
	"log"
	"os"
)

func main() {
	if err := mcp.Run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
