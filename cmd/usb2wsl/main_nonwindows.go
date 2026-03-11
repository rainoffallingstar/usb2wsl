//go:build !windows

package main

import "log"

func main() {
	log.Fatal("usb2wsl only builds/runs on Windows")
}

