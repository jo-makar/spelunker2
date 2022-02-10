package main

import (
	"golang.org/x/net/websocket"

	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Chrome struct {
	cmd    *exec.Cmd
	done   chan bool

	ws     *websocket.Conn

	mux    sync.Mutex

	msgs   []message
	msgidx int             // Write index
	msgcnt int             // Item count 
	msgmux sync.Mutex

	reqid  int

	// Options specific to Evaluate* methods
	evalopts struct {
		timeout time.Duration
	}
}

type message struct {
	Received time.Time

	// Response fields
	Id       int         `json:"id"`
	Result   interface{} `json:"result"`

	// Event fields
	Method   string      `json:"method"`
	Params   interface{} `json:"params"`
}

func NewChrome(incognito bool) (*Chrome, error) {
	//
	// Launch headless Chrome
	//
	
	chrome := Chrome{
		msgs: make([]message, 1000),
	}

	cmd := []string{"/opt/google/chrome/chrome", "--headless", "--remote-debugging-port=0"}

	if home, ok := os.LookupEnv("HOME"); !ok {
		return nil, errors.New("HOME not defined")
	} else {
		cmd = append(cmd, fmt.Sprintf("--user-data-dir=%s/.config/google-chrome", home))
	}

	if incognito {
		cmd = append(cmd, "--incognito")
	}


	chrome.cmd = exec.Command(cmd[0], cmd[1:]...)
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

	//
	// Retrieve and connect to the debugger url (via a websocket)
	// Also process stdout/stderr output (necessary to retrieve the debugger url)
	//

	devtools := make(chan string)

	for index, reader := range []io.ReadCloser{stdout, stderr} {
		go func() {
			sent := false
			output := func(line string) {
				if len(line) > 0 {
					DebugLog("chrome/%d: %s", index + 1, line)

					if index == 1 && !sent {
						prefix := "DevTools listening on "
						if strings.HasPrefix(line, prefix) {
							devtools <- line[len(prefix):]
							close(devtools)
							sent = true
						}
					}
				}
			}

			var linebuf bytes.Buffer
			buf := make([]byte, 1024)

			for {
				n, err := reader.Read(buf)
				if err != nil {
					if !(errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed)) {
						ErrorLog("<%d>.Read(): %s", index + 1, err.Error())
					}
					break
				}
				if n > 0 {
					splits := bytes.SplitAfter(buf[:n], []byte{'\n'})
					if len(splits) == 1 {
						linebuf.Write(buf[:n])
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

	var devtoolsUrl string

	select {
	case <- chrome.done:
		return nil, errors.New("chrome exited early")
	case devtoolsUrl = <- devtools:
		InfoLog("devtools url: %s", devtoolsUrl)
	case <- time.After(15 * time.Second):
		chrome.Close()
		return nil, errors.New("deadline reached looking for devtools url")
	}

	parsedUrl, err := url.Parse(devtoolsUrl)
	if err != nil {
		chrome.Close()
		return nil, err
	}

	parsedUrl.Scheme = "http"
	parsedUrl.Path = "/json"

	time.Sleep(5 * time.Second)

	client := http.Client{ Timeout: 15 * time.Second }
	req, err := http.NewRequest("GET", parsedUrl.String(), nil)
	if err != nil {
		chrome.Close()
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		chrome.Close()
		return nil, err
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		chrome.Close()
		return nil, fmt.Errorf("bad status code (%d) from %s", resp.StatusCode, parsedUrl.String())
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		chrome.Close()
		return nil, err
	}

	type entry struct {
		DebuggerUrl string `json:"webSocketDebuggerUrl"`
	}

	var entries []entry
	if err := json.Unmarshal(body, &entries); err != nil {
		chrome.Close()
		return nil, err
	}

	if len(entries) != 1 {
		chrome.Close()
		return nil, fmt.Errorf("unexpected entry count (%d)", len(entries))
	}
	debuggerUrl := entries[0].DebuggerUrl
	InfoLog("debugger url: %s", debuggerUrl)

	chrome.ws, err = websocket.Dial(debuggerUrl, "", "http://127.0.0.1/")
	if err != nil {
		chrome.Close()
		return nil, err
	}

	//
	// Define a goroutine to process websocket messages
	// To be held in a FIFO circular buffer
	//
	
	go func() {
		var msgbuf bytes.Buffer
		chunk := make([]byte, 4096)

		for {
			n, err := chrome.ws.Read(chunk)
			if err != nil {
				//if !errors.Is(err, net.ErrClosed) {
				if !(strings.HasSuffix(err.Error(), "use of closed network connection") || errors.Is(err, io.EOF)) {
					ErrorLog(err)
				}
				break
			}
			if n > 0 {
				var msg message

				msgbuf.Write(chunk[:n])
				if err := json.Unmarshal(msgbuf.Bytes(), &msg); err != nil {
					continue
				}

				TraceLog("<<< %s", msgbuf.String())
				msgbuf.Reset()

				// Not all events include a timestamp param
				msg.Received = time.Now()

				chrome.msgmux.Lock()

				chrome.msgs[chrome.msgidx] = msg

				chrome.msgidx = (chrome.msgidx + 1) % len(chrome.msgs)
				if chrome.msgcnt < len(chrome.msgs) {
					chrome.msgcnt++
				}

				chrome.msgmux.Unlock()
			}
		}
	}()

	//
	// Debugger initialization
	//

	// FIXME stopped
	chrome.EvaluateString("navigator.userAgent", nil)

	return &chrome, nil
}

func (c *Chrome) Close() error {
	c.mux.Lock()
	defer c.mux.Unlock()

	done := false
	if c.ws != nil {
		if _, err := c.execute("Browser.close", nil, -1); err != nil {
			ErrorLog(err)
		}

		select {
		case <- c.done:
			done = true
		case <- time.After(15 * time.Second):
			WarningLog("deadline reached waiting for Browser.close")
		}

		if err := c.ws.Close(); err != nil {
			ErrorLog(err)
		}
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

func (c *Chrome) execute(method string, params map[string]interface{}, timeout time.Duration) (interface{}, error) {
	req := struct {
		Id     int                    `json:"id"`
		Method string                 `json:"method"`
		Params map[string]interface{} `json:"params,omitempty"`
	}{
		Id:     c.reqid,
		Method: method,
		Params: params,
	}
	c.reqid++

	reqbytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	TraceLog(">>> %s", reqbytes)

	if _, err = c.ws.Write(reqbytes); err != nil {
		return nil, err
	}

	// timeout < 0 => don't wait for a response
	// timeout = 0 => wait for a response indefinitely
	// timeout > 0 => wait until (at most) timeout

	if timeout < 0 {
		return nil, nil
	}

	stop := time.Now().Add(timeout)
	for {
		if timeout > 0 && time.Now().After(stop) {
			return nil, errors.New("execution timed out")
		}

		c.msgmux.Lock()

		if c.msgcnt > 0 {
			var i, j int
			if c.msgcnt == len(c.msgs) {
				i = c.msgidx
				j = (c.msgidx + 1) % len(c.msgs)
			} else {
				i = 0
				j = c.msgcnt - 1
			}

			k := i
			for {
				if c.msgs[k].Id == req.Id {
					result := c.msgs[k].Result
					c.msgmux.Unlock()
					return result, nil
				}

				if k == j {
					break
				}
				k = (i + 1) % len(c.msgs)
			}
		}

		c.msgmux.Unlock()

		time.Sleep(100 * time.Millisecond)
	}
}

func (c *Chrome) evaloptsUpdate(options map[string]interface{}) {
	c.evalopts.timeout = 5 * time.Second
	if v, ok := options["timeout"]; ok {
		if w, ok := v.(time.Duration); ok {
			c.evalopts.timeout = w
		}
	}
}

func (c *Chrome) Evaluate(expr string, options map[string]interface{}) error {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.evaloptsUpdate(options)

	_, err := c.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, c.evalopts.timeout)
	return err
}

func evalresultExtract(result interface{}) (string, interface{}, error) {
	resultmap, ok := result.(map[string]interface{})
	if !ok {
		return "", nil, errors.New("result type assertion failed")
	}

	resultresultiface, ok := resultmap["result"]
	if !ok {
		return "", nil, errors.New("result[\"result\"] missing")
	}

	resultresultmap, ok := resultresultiface.(map[string]interface{})
	if !ok {
		return "", nil, errors.New("result[\"result\"] type assertion failed")
	}

	typeiface, ok := resultresultmap["type"]
	if !ok {
		return "", nil, errors.New("result[\"result\"][\"type\"] missing")
	}
	typestr, ok := typeiface.(string)
	if !ok {
		return "", nil, errors.New("result[\"result\"][\"type\"] type assertion failed")
	}

	valueiface, ok := resultresultmap["value"]
	if !ok {
		return "", nil, errors.New("result[\"result\"][\"value\"] missing")
	}

	return typestr, valueiface, nil
}

func (c *Chrome) EvaluateBool(expr string, options map[string]interface{}) (bool, error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.evaloptsUpdate(options)

	result, err := c.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, c.evalopts.timeout)
	if err != nil {
		return false, err
	}
	typestr, valueiface, err := evalresultExtract(result)
	if err != nil {
		return false, err
	}

	if typestr != "boolean" {
		return false, fmt.Errorf("unexpected type: %s", typestr)
	}
	if value, ok := valueiface.(bool); !ok {
		return false, errors.New("value type assertion failed")
	} else {
		return value, nil
	}
}

func (c *Chrome) EvaluateInt(expr string, options map[string]interface{}) (int, error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.evaloptsUpdate(options)

	result, err := c.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, c.evalopts.timeout)
	if err != nil {
		return 0, err
	}
	typestr, valueiface, err := evalresultExtract(result)
	if err != nil {
		return 0, err
	}

	if typestr != "number" {
		return 0, fmt.Errorf("unexpected type: %s", typestr)
	}
	if value, ok := valueiface.(int); !ok {
		return 0, errors.New("value type assertion failed")
	} else {
		return value, nil
	}
}

func (c *Chrome) EvaluateFloat(expr string, options map[string]interface{}) (float64, error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.evaloptsUpdate(options)

	result, err := c.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, c.evalopts.timeout)
	if err != nil {
		return 0.0, err
	}
	typestr, valueiface, err := evalresultExtract(result)
	if err != nil {
		return 0.0, err
	}

	if typestr != "number" {
		return 0.0, fmt.Errorf("unexpected type: %s", typestr)
	}
	if value, ok := valueiface.(float64); !ok {
		return 0.0, errors.New("value type assertion failed")
	} else {
		return value, nil
	}
}

func (c *Chrome) EvaluateString(expr string, options map[string]interface{}) (string, error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.evaloptsUpdate(options)

	result, err := c.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, c.evalopts.timeout)
	if err != nil {
		return "", err
	}
	typestr, valueiface, err := evalresultExtract(result)
	if err != nil {
		return "", err
	}

	if typestr != "string" {
		return "", fmt.Errorf("unexpected type: %s", typestr)
	}
	if value, ok := valueiface.(string); !ok {
		return "", errors.New("value type assertion failed")
	} else {
		return value, nil
	}
}

// TODO Add support for simultaneous sessions
