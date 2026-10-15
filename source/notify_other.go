//go:build !windows

package main

import "fmt"

func notify(title, message string) { fmt.Println(title + "\n" + message) }
