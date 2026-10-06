//go:build !windows

package main

import "fmt"

// The app only runs on Windows; this stub lets the portable logic be built and tested anywhere.
func main() { fmt.Println("Deej Mixer runs on Windows. Build with GOOS=windows.") }
