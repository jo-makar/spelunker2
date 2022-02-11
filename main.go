package main

import (
)

func main() {
	browser, err := NewBrowser(false)
	if err != nil {
		PanicLog(err)
	}
	defer browser.Close()

	for job := range LinkedIn(browser, nil) {
		InfoLog("%+v", job)
	}
}
