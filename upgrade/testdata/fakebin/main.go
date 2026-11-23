// Command fakebin stands in for a jikunode release binary in upgrade tests:
// "version" prints the linked version, anything else idles like a running
// node so the test can hold the executable open.
package main

import (
	"fmt"
	"os"
	"time"
)

var Version = "v0.0.0"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("jikunode " + Version)
		return
	}
	time.Sleep(time.Minute)
}
