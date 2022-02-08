package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Chrome struct {
	cmd  *exec.Cmd
	done chan bool

	mux  sync.Mutex
}

func NewChrome(incognito bool) (*Chrome, error) {
	chrome := Chrome{}

	cmd := []string{"/opt/google/chrome/chrome", "--headless", "--remote-debugging-port=0"}

	if home, ok := os.LookupEnv("HOME"); !ok {
		return nil, errors.New("HOME not defined")
	} else {
		cmd = append(cmd, fmt.Sprintf("--user-data-dir=%s/.config/google-chrome", home))
	}

	if incognito {
		cmd = append(cmd, "--incognito")
	}


	chrome.cmd = exec.Command(cmd[0], cmd[1:len(cmd)]...)
	stdout, err := chrome.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := chrome.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := chrome.cmd.Start(); err != nil {
		return nil, err
	}
	InfoLog("chrome process launched (pid %d)\n", chrome.cmd.Process.Pid)

	chrome.done = make(chan bool)
	go func() {
		if err := chrome.cmd.Wait(); err != nil {
			if exiterr, ok := err.(*exec.ExitError); ok {
				if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
					if status.Exited() {
						InfoLog("cmd.Wait(): exited with return code %d", status.ExitStatus())
					} else if status.Signaled() {
						InfoLog("cmd.Wait(): %s (%d) signaled", status.Signal().String(), status.Signal())
					} else {
						ErrorLog("cmd.Wait(): unhandled status: %s", exiterr.Error())
					}
				} else {
					ErrorLog("cmd.Wait(): unhandled type: %s", exiterr.Error())
				}
			} else {
				ErrorLog("cmd.Wait(): %s\n", err.Error())
			}
		}
		chrome.done <- true
	}()

	for index, reader := range []io.ReadCloser{stdout, stderr} {
	    go func() {
		    output := func(line string) {
			    if len(line) > 0 {
				    DebugLog("chrome/%d: %s", index + 1, line)
			    }
		    }

		    var linebuf bytes.Buffer
		    for {
			    buf := make([]byte, 1024)
			    n, err := reader.Read(buf)
			    if err != nil {
				    if !(errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed)) {
					    ErrorLog("<%d>.Read(): %s", index + 1, err.Error())
				    }
				    break
			    }
			    if n > 0 {
				    splits := bytes.SplitAfter(buf[0:n], []byte{'\n'})
				    if len(splits) == 1 {
					    linebuf.Write(buf[0:n])
				    } else {
					    output(linebuf.String() + strings.TrimSuffix(string(splits[0]), "\n"))
					    for i := 1; i < len(splits) - 1; i++ {
						    output(strings.TrimSuffix(string(splits[i]), "\n"))
					    }

					    linebuf.Reset()
					    linebuf.Write(splits[len(splits) - 1])
				    }
			    }
		    }
	    }()
	}

	// FIXME grab and store the devtools url above

	return &chrome, nil
}

func (c *Chrome) Close() error {
	c.mux.Lock()
	defer c.mux.Unlock()

	// FIXME Browser.close

	done := false
	select {
	case <- c.done:
		done = true
	case <- time.After(15 * time.Second):
		WarningLog("deadline reached waiting for Browser.close")
	}

	if !done {
		if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
			return err
		}

		select {
		case <- c.done:
			done = true
		case <- time.After(15 * time.Second):
			WarningLog("deadline reached waiting for SIGINT")
			if err := c.cmd.Process.Kill(); err != nil {
				return err
			}
		}

	}

	return nil
}
