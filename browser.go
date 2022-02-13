package main

import (
	"golang.org/x/net/websocket"

	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Browser struct {
	cmd    *exec.Cmd
	done   chan bool

	ws     *websocket.Conn

	mux    sync.Mutex

	msgs   []message
	msgidx int             // Write index
	msgcnt int             // Item count 
	msgmux sync.Mutex

	reqid  int
}

// Messages are dynamic json structures divided into responses and events
// Ie only a subset of the following fields will be populated at once
type message struct {
	Received time.Time

	//
	// Response fields
	//

	Id int `json:"id"`
	Result struct {
		Result struct {
			Type  string      `json:"type"`
			Value interface{} `json:"value"`
		} `json:"result"`

		FrameId string `json:"frameId"`
	} `json:"result"`

	//
	// Event fields
	//

	Method string `json:"method"`
	Params struct {
		Frame struct {
			Id string `json:id"`
		} `json:"frame"`
	} `json:"params"`
}

func NewBrowser(incognito bool) (*Browser, error) {
	//
	// Launch headless browser
	//
	
	browser := Browser{
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


	browser.cmd = exec.Command(cmd[0], cmd[1:]...)
	stdout, err := browser.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := browser.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := browser.cmd.Start(); err != nil {
		return nil, err
	}
	InfoLog("browser process launched (pid %d)\n", browser.cmd.Process.Pid)

	browser.done = make(chan bool)

	go func() {
		if err := browser.cmd.Wait(); err != nil {
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
		browser.done <- true
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
					DebugLog("browser/%d: %s", index + 1, line)

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
	case <- browser.done:
		return nil, errors.New("browser exited early")
	case devtoolsUrl = <- devtools:
		DebugLog("devtools url: %s", devtoolsUrl)
	case <- time.After(15 * time.Second):
		browser.Close()
		return nil, errors.New("deadline reached looking for devtools url")
	}

	parsedUrl, err := url.Parse(devtoolsUrl)
	if err != nil {
		browser.Close()
		return nil, err
	}

	parsedUrl.Scheme = "http"
	parsedUrl.Path = "/json"

	time.Sleep(5 * time.Second)

	client := http.Client{ Timeout: 15 * time.Second }
	req, err := http.NewRequest("GET", parsedUrl.String(), nil)
	if err != nil {
		browser.Close()
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		browser.Close()
		return nil, err
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		browser.Close()
		return nil, fmt.Errorf("bad status code (%d) from %s", resp.StatusCode, parsedUrl.String())
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		browser.Close()
		return nil, err
	}

	type entry struct {
		DebuggerUrl string `json:"webSocketDebuggerUrl"`
	}

	var entries []entry
	if err := json.Unmarshal(body, &entries); err != nil {
		browser.Close()
		return nil, err
	}

	if len(entries) != 1 {
		browser.Close()
		return nil, fmt.Errorf("unexpected entry count (%d)", len(entries))
	}
	debuggerUrl := entries[0].DebuggerUrl
	DebugLog("debugger url: %s", debuggerUrl)

	browser.ws, err = websocket.Dial(debuggerUrl, "", "http://127.0.0.1/")
	if err != nil {
		browser.Close()
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
			n, err := browser.ws.Read(chunk)
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

				browser.msgmux.Lock()

				browser.msgs[browser.msgidx] = msg

				browser.msgidx = (browser.msgidx + 1) % len(browser.msgs)
				if browser.msgcnt < len(browser.msgs) {
					browser.msgcnt++
				}

				browser.msgmux.Unlock()
			}
		}
	}()

	//
	// Debugger initialization
	//

	timeout := 5 * time.Second

	useragent, err := browser.EvaluateString("navigator.userAgent", nil)
	if err != nil {
		browser.Close()
		return nil, err
	}
	useragent = strings.Replace(useragent, "HeadlessChrome", "Chrome", 1)
	if _, err := browser.execute("Network.setUserAgentOverride", map[string]interface{}{ "userAgent": useragent }, timeout); err != nil {
		browser.Close()
		return nil, err
	}

	// Enable notification of Page domain events
	// TODO Consider also enabling notification of DOM and Network events
	if _, err := browser.execute("Page.enable", nil, timeout); err != nil {
		browser.Close()
		return nil, err
	}

	return &browser, nil
}

func (b *Browser) Close() error {
	b.mux.Lock()
	defer b.mux.Unlock()

	done := false
	if b.ws != nil {
		if _, err := b.execute("Browser.close", nil, -1); err != nil {
			ErrorLog(err)
		}

		select {
		case <- b.done:
			done = true
		case <- time.After(15 * time.Second):
			WarningLog("deadline reached waiting for Browser.close")
		}

		if err := b.ws.Close(); err != nil {
			ErrorLog(err)
		}
	}

	if !done {
		if err := b.cmd.Process.Signal(os.Interrupt); err != nil {
			return err
		}

		select {
		case <- b.done:
			done = true
		case <- time.After(15 * time.Second):
			WarningLog("deadline reached waiting for SIGINT")
			if err := b.cmd.Process.Kill(); err != nil {
				return err
			}
		}

	}

	return nil
}

func (b *Browser) waitfor(predicate func(m *message) bool, timeout time.Duration) (*message, error) {
	stop := time.Now().Add(timeout)
	for {
		if timeout > 0 && time.Now().After(stop) {
			return nil, nil
		}

		b.msgmux.Lock()

		if b.msgcnt > 0 {
			var i, j int
			if b.msgcnt == len(b.msgs) {
				i = b.msgidx
				j = (b.msgidx + 1) % len(b.msgs)
			} else {
				i = 0
				j = b.msgcnt - 1
			}

			k := i
			for {
				if predicate(&b.msgs[k]) {
					msg := b.msgs[k]
					b.msgmux.Unlock()
					return &msg, nil
				}

				if k == j {
					break
				}
				k = (k + 1) % len(b.msgs)
			}
		}

		b.msgmux.Unlock()

		time.Sleep(100 * time.Millisecond)
	}
}

func (b *Browser) execute(method string, params map[string]interface{}, timeout time.Duration) (*message, error) {
	req := struct {
		Id     int                    `json:"id"`
		Method string                 `json:"method"`
		Params map[string]interface{} `json:"params,omitempty"`
	}{
		Id:     b.reqid,
		Method: method,
		Params: params,
	}
	b.reqid++

	reqbytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	TraceLog(">>> %s", reqbytes)

	if _, err = b.ws.Write(reqbytes); err != nil {
		return nil, err
	}

	// timeout < 0 => don't wait for a response
	// timeout = 0 => wait for a response indefinitely
	// timeout > 0 => wait until (at most) timeout

	if timeout < 0 {
		return nil, nil
	}

	if msg, err := b.waitfor(func(m *message) bool { return m.Id == req.Id }, timeout); err != nil {
		return nil, err
	} else {
		if msg == nil {
			return nil, errors.New("execution timed out")
		} else {
			return msg, nil
		}
	}
}

func (b *Browser) Evaluate(expr string, options map[string]interface{}) error {
	b.mux.Lock()
	defer b.mux.Unlock()

	timeout := 5 * time.Second
	if t, ok := options["timeout"].(time.Duration); ok {
		timeout = t
	}

	_, err := b.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, timeout)
	return err
}

func (b *Browser) EvaluateBool(expr string, options map[string]interface{}) (bool, error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	timeout := 5 * time.Second
	if t, ok := options["timeout"].(time.Duration); ok {
		timeout = t
	}

	msg, err := b.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, timeout)
	if err != nil {
		return false, err
	}

	if msg.Result.Result.Type != "boolean" {
		return false, fmt.Errorf("unexpected type: %s", msg.Result.Result.Type)
	}
	if value, ok := msg.Result.Result.Value.(bool); !ok {
		return false, errors.New("value type assertion failed")
	} else {
		return value, nil
	}
}

func (b *Browser) EvaluateInt(expr string, options map[string]interface{}) (int, error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	timeout := 5 * time.Second
	if t, ok := options["timeout"].(time.Duration); ok {
		timeout = t
	}

	msg, err := b.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, timeout)
	if err != nil {
		return 0, err
	}

	if msg.Result.Result.Type != "number" {
		return 0, fmt.Errorf("unexpected type: %s", msg.Result.Result.Type)
	}
	if value, ok := msg.Result.Result.Value.(float64); !ok {
		return 0, errors.New("value type assertion failed")
	} else {
		if math.Floor(value) != value {
			return 0, errors.New("value type check failed")
		} else {
			return int(value), nil
		}
	}
}

func (b *Browser) EvaluateFloat(expr string, options map[string]interface{}) (float64, error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	timeout := 5 * time.Second
	if t, ok := options["timeout"].(time.Duration); ok {
		timeout = t
	}

	msg, err := b.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, timeout)
	if err != nil {
		return 0.0, err
	}

	if msg.Result.Result.Type != "number" {
		return 0.0, fmt.Errorf("unexpected type: %s", msg.Result.Result.Type)
	}
	if value, ok := msg.Result.Result.Value.(float64); !ok {
		return 0.0, errors.New("value type assertion failed")
	} else {
		return value, nil
	}
}

func (b *Browser) EvaluateString(expr string, options map[string]interface{}) (string, error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	timeout := 5 * time.Second
	if t, ok := options["timeout"].(time.Duration); ok {
		timeout = t
	}

	msg, err := b.execute("Runtime.evaluate", map[string]interface{}{ "expression": expr }, timeout)
	if err != nil {
		return "", err
	}

	if msg.Result.Result.Type != "string" {
		return "", fmt.Errorf("unexpected type: %s", msg.Result.Result.Type)
	}
	if value, ok := msg.Result.Result.Value.(string); !ok {
		return "", errors.New("value type assertion failed")
	} else {
		return value, nil
	}
}

func (b *Browser) Goto(url string, options map[string]interface{}) (bool, error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	cmdTimeout := 5 * time.Second

	gotoTimeout := 60 * time.Second
	if t, ok := options["timeout"].(time.Duration); ok {
		gotoTimeout = t
	}

	var referrer string
	if r, ok := options["referrer"].(string); ok {
		referrer = r
	}

	params := map[string]interface{}{ "url": url }
	if referrer != "" {
		params["referrer"] = referrer
	}

	msg, err := b.execute("Page.navigate", params, cmdTimeout)
	if err != nil {
		return false, err
	}
	if msg.Result.FrameId == "" {
		return false, errors.New("frameId missing")
	}
	DebugLog("%+v", msg.Result.FrameId)

	pred := func(m *message) bool { return m.Method == "Page.frameNavigated" && m.Params.Frame.Id == msg.Result.FrameId }
	if evt, err := b.waitfor(pred, gotoTimeout); err != nil {
		return false, err
	} else if evt == nil {
		return true, nil
	} else {
		return false, nil
	}
}

// TODO Add support for simultaneous sessions
