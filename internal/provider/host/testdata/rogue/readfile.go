//go:build readfile

package main

import "os"

func init() {
	readFile = func() string {
		b, _ := os.ReadFile("/etc/hosts")
		return string(b)
	}
}
