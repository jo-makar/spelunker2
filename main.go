package main

import (
	"time"
)

func main() {
	chrome, err := NewChrome(false)
	if err != nil {
		PanicLog(err)
	}
	defer chrome.Close()

	time.Sleep(15 * time.Second)
}
