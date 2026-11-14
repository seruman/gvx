// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command unixdial connects to the Unix socket of its first argument, sends
// its second argument, closes its write side, and prints the reply. With the
// arguments "stat PATH" it prints the permission bits of PATH in octal. With
// "hold PATH" it connects, sends one byte, and exits without a close.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "stat" {
		info, err := os.Stat(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "stat:", err)
			os.Exit(1)
		}
		fmt.Printf("%o", info.Mode().Perm())
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "hold" {
		c, err := net.Dial("unix", os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "dial:", err)
			os.Exit(1)
		}
		_, _ = c.Write([]byte{'x'})
		os.Exit(0)
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: unixdial PATH MESSAGE")
		os.Exit(2)
	}
	c, err := net.Dial("unix", os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	if _, err := io.WriteString(c, os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		fmt.Fprintln(os.Stderr, "close write:", err)
		os.Exit(1)
	}
	reply, err := io.ReadAll(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	fmt.Print(string(reply))
}
