use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;

use regex::Regex;

use reqwest::{self, StatusCode};

use serde::{Deserialize, Serialize};

use serde_json::{self, Map, Number, Value};

use std::env;
use std::error::Error;
use std::fmt::{self, Debug, Formatter};
use std::io::{self, BufRead, BufReader};
use std::net::TcpStream;
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use timeout_readwrite::TimeoutReader;

use tungstenite::{self, protocol::{self, WebSocket}};

use url::Url;

#[derive(Clone, Debug, Deserialize)]
struct ResponseResultResult {
    #[serde(rename = "type")]
    type_: String,
    value: Value,
}

#[derive(Clone, Debug, Deserialize)]
struct ResponseResult {
    result: Option<ResponseResultResult>,
    #[serde(rename = "frameId")]
    frame_id: Option<String>,
    #[serde(rename = "loaderId")]
    loader_id: Option<String>,
}

#[derive(Clone, Debug, Deserialize)]
struct Response {
    id: u64,
    result: ResponseResult,
}

#[derive(Debug, Deserialize)]
struct EventParamsFrame {
    id: String,
    #[serde(rename = "loaderId")]
    loader_id: String,
}

#[derive(Debug, Deserialize)]
struct EventParams {
    frame: EventParamsFrame,
}

#[derive(Debug, Deserialize)]
struct Event {
    method: String,
    params: EventParams,
}

#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum Message {
    Response(Response),
    Event(Event),
}

struct BrowserShared {
    websocket: WebSocket<TcpStream>,
    ringbuf: RingBuf<Message>,
}

pub struct Browser {
    child: Child,
    shared: Arc<Mutex<BrowserShared>>,
    request_id: u64,
}

impl Browser {
    pub fn new() -> Result<Self, Box<dyn Error>> {
        let mut cmd = Command::new("/opt/google/chrome/chrome");
        cmd.args(&["--headless", "--remote-debugging-port=0"]);

        match env::var("HOME") {
            Ok(h) => { cmd.arg(format!("--user-data-dir={}/.config/google-chrome", h)); },
            Err(e) => return Err(Box::new(e))
        }

        cmd.stdin(Stdio::null())
           .stdout(Stdio::null())
           .stderr(Stdio::piped());

        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => return Err(Box::new(e))
        };

        let stderr = child.stderr.take().unwrap();

        // Ideally would unwrap stderr from the BufReader and TimeoutReader for later use in the thread below.
        // However (at least currently) only BufReader supports into_inner() to unwrap the underlying reader.
        let mut stderr_reader = BufReader::new(TimeoutReader::new(stderr, Duration::from_secs(1)));

        let mut port: Option<u16> = None;
        {
            let re = Regex::new(r"^DevTools listening on (ws://127.0.0.1:(\d+)/devtools/browser/[a-f0-9-]+)$").unwrap();

            for _ in 0..15 {
                let mut line = String::new();
                match stderr_reader.read_line(&mut line) {
                    Ok(_) => {
                        if line.ends_with('\n') {
                            line.pop();
                        }
                        if !line.is_empty() {
                            log::info!("stderr: {}", line);

                            if let Some(c) = re.captures(&line) {
                                log::info!("websocket url: {}", c.get(1).unwrap().as_str());

                                let s = c.get(2).unwrap().as_str();
                                match s.parse::<u16>() {
                                    Ok(p) => {
                                        port = Some(p);
                                        break;
                                    },
                                    Err(e) => log::error!("error parsing port {}: {}", s, e)
                                }
                            }
                        }
                    },
                    Err(e) if e.kind() == io::ErrorKind::TimedOut => {},
                    Err(e) => {
                        log::error!("error reading stderr: {}", e);
                        break;
                    }
                }
            }

            if let None = port {
                let _ = child.kill();
                return Err(string_error::static_err("websocket url not found"));
            }
        }
        let port = port.unwrap();

        let debugger_url: String;
        {
            #[derive(Debug, Deserialize)]
            struct Config {
                #[serde(rename = "webSocketDebuggerUrl")]
                debugger_url: String,
            }

            match reqwest::blocking::get(format!("http://127.0.0.1:{}/json", port)) {
                Ok(r) if r.status() == StatusCode::OK => {
                    match r.json::<Vec<Config>>() {
                        Ok(c) if c.len() == 1 => {
                            debugger_url = c[0].debugger_url.clone();
                            log::info!("debugger url: {}", debugger_url);
                        },
                        Ok(_) => {
                            let _ = child.kill();
                            return Err(string_error::static_err("unexpected json url response config count"));
                        },
                        Err(e) => {
                            let _ = child.kill();
                            return Err(Box::new(e));
                        }
                    }
                },
                Ok(r) => {
                    let _ = child.kill();
                    return Err(string_error::into_err(format!("{} status from json url", r.status())));
                },
                Err(e) => {
                    let _ = child.kill();
                    return Err(Box::new(e));
                }
            }
        }

        thread::spawn(move || {
            for line in stderr_reader.lines() {
                match line {
                    Ok(l) => {
                        if !l.is_empty() {
                            log::info!("stderr: {}", l);
                        }
                    },
                    Err(e) if e.kind() == io::ErrorKind::TimedOut => {},
                    Err(e) => {
                        log::error!("error reading stderr: {}", e);
                        break;
                    }
                }
            }
        });

        // Providing a non-blocking stream so that websocket will support non-blocking reads.
        // Ref: https://docs.rs/tungstenite/0.17.2/tungstenite/client/fn.client.html
        //      https://doc.rust-lang.org/stable/std/net/struct.TcpStream.html#method.set_nonblocking
        let stream: TcpStream;
        {
            let port = match Url::parse(&debugger_url) {
                Ok(u) => u.port().unwrap_or(80),
                Err(e) => {
                    let _ = child.kill();
                    return Err(Box::new(e));
                }
            };

            stream = match TcpStream::connect(format!("127.0.0.1:{}", port)) {
                Ok(s) => s,
                Err(e) => {
                    let _ = child.kill();
                    return Err(Box::new(e));
                }
            };
        }

        let mut websocket: WebSocket<TcpStream>;
        match tungstenite::client::client(debugger_url, stream) {
            Ok((w, r)) if r.status() == StatusCode::SWITCHING_PROTOCOLS => websocket = w,
            Ok((_, r)) => {
                let _ = child.kill();
                return Err(string_error::into_err(format!("{} status from debugger url", r.status())));
            },
            Err(e) => {
                let _ = child.kill();
                return Err(Box::new(e));
            }
        }

        if let Err(e) = websocket.get_mut().set_nonblocking(true) {
            let _ = child.kill();
            return Err(Box::new(e));
        }

        let shared = Arc::new(Mutex::new(BrowserShared{
            websocket: websocket,
            ringbuf: RingBuf::<Message>::new(),
        }));

        {
            let shared = shared.clone();
            thread::spawn(move || {
                loop {
                    match shared.lock() {
                        Ok(ref mut s) => {
                            match s.websocket.read_message() {
                                Ok(tungstenite::protocol::Message::Text(m)) => {
                                    log::trace!("<<< {}", m);
                                    match serde_json::from_str::<Message>(&m) {
                                        Ok(m) => s.ringbuf.push(m),
                                        Err(_e) => {
                                            // Only a single event format is defined by Message::Event enum.
                                            // Add more (ideally all possible) for thie error message to be useful.
                                            // log::error!("json deserialization error: {:?}", _e);
                                        }
                                    }
                                },
                                Ok(_) => log::warn!("received non-text websocket message"),
                                Err(tungstenite::error::Error::Io(e)) if e.kind() == io::ErrorKind::WouldBlock => {},
                                Err(tungstenite::error::Error::Protocol(tungstenite::error::ProtocolError::ResetWithoutClosingHandshake)) => break,
                                Err(e) => {
                                    log::error!("websocket error: {}", e);
                                    break;
                                }
                            }
                        },
                        Err(e) => {
                            log::error!("poisoned mutex: {}", e);
                            break;
                        }
                    }
                    thread::sleep(Duration::from_millis(100));
                }
            });
        }

        let mut retval = Self {
            child: child,
            shared: shared,
            request_id: 0,
        };

        {
            let mut user_agent = match retval.evaluate_string("navigator.userAgent", Some(Duration::from_secs(5))) {
                Ok(s) => s,
                Err(e) => {
                    drop(retval);
                    return Err(e);
                }
            };
            user_agent = user_agent.replacen("HeadlessChrome", "Chrome", 1);

            let mut params: Map<String, Value> = Map::new();
            params.insert("userAgent".to_string(), Value::String(user_agent.to_string()));
            if let Err(e) = retval.execute("Network.setUserAgentOverride", Some(params), Some(Duration::from_secs(5))) {
                drop(retval);
                return Err(e);
            }

            if let Err(e) = retval.execute("Page.enable", None, Some(Duration::from_secs(5))) {
                drop(retval);
                return Err(e);
            }
        }

        Ok(retval)
    }

    fn execute(&mut self, method: &str, params: Option<Map<String, Value>>, timeout: Option<Duration>) -> Result<Response, Box<dyn Error>> {
        #[derive(Serialize)]
        struct Request {
            id: Number,
            method: String,
            #[serde(skip_serializing_if = "Option::is_none")]
            params: Option<Map<String, Value>>,
        }

        let request = Request{
            id: Number::from(self.request_id),
            method: method.to_string(),
            params: params,
        };
        self.request_id += 1;

        let shared = self.shared.clone();

        {
            let request_string = match serde_json::to_string(&request) {
                Ok(s) => s,
                Err(e) => return Err(Box::new(e))
            };
            log::trace!(">>> {}", request_string);

            match shared.lock() {
                Ok(mut s) => {
                    match s.send_request(&request_string) {
                        Ok(_) => {},
                        Err(e) => return Err(e)
                    }
                },
                Err(e) => return Err(string_error::into_err(format!("poisoned mutex: {}", e)))
            }
        }

        {
            let start = Instant::now();
            loop {
                if let Some(t) = timeout {
                    if Instant::now().duration_since(start) > t {
                        return Err(Box::new(io::Error::new(io::ErrorKind::TimedOut, "timed out")));
                    }
                }

                match shared.lock() {
                    Ok(s) => {
                        for message in s.ringbuf.iter() {
                            if let Message::Response(r) = message {
                                if let Some(i) = request.id.as_u64() {
                                    if r.id == i {
                                        return Ok(r.clone());
                                    }
                                }
                            }
                        }
                    },
                    Err(e) => return Err(string_error::into_err(format!("poisoned mutex: {}", e)))
                }

                thread::sleep(Duration::from_millis(100));
            }
        }
    }

    pub fn evaluate_string(&mut self, expr: &str, timeout: Option<Duration>) -> Result<String, Box<dyn Error>> {
        let mut params: Map<String, Value> = Map::new();
        params.insert("expression".to_string(), Value::String(expr.to_string()));
        match self.execute("Runtime.evaluate", Some(params), timeout) {
            Ok(r) => {
                if let Some(s) = r.result.result {
                    if s.type_ == "string" {
                        Ok(s.value.to_string())
                    } else {
                        Err(string_error::into_err(format!("response result wrong type: {:?}", s.type_)))
                    }
                } else {
                    Err(string_error::into_err(format!("response missing result: {:?}", r)))
                }
            },
            Err(e) => Err(e)
        }
    }

    pub fn goto(&mut self, url: &str, timeout: Option<Duration>, referrer: Option<&str>) -> Result<(), Box<dyn Error>> {
        let mut params: Map<String, Value> = Map::new();
        params.insert("url".to_string(), Value::String(url.to_string()));
        if let Some(r) = referrer {
            params.insert("referrer".to_string(), Value::String(r.to_string()));
        }

        let (frame_id, loader_id): (String, String);
        match self.execute("Page.navigate", Some(params), Some(Duration::from_secs(5))) {
            Ok(r) => {
                if let Some(s) = r.result.frame_id {
                    frame_id = s;
                } else {
                    return Err(string_error::static_err("response result missing frame_id"));
                }
                if let Some(s) = r.result.loader_id {
                    loader_id = s;
                } else {
                    return Err(string_error::static_err("response result missing loader_id"));
                }
            },
            Err(e) => return Err(e)
        }

        let shared = self.shared.clone();
        {
            let start = Instant::now();
            loop {
                if let Some(t) = timeout {
                    if Instant::now().duration_since(start) > t {
                        return Err(Box::new(io::Error::new(io::ErrorKind::TimedOut, "timed out")));
                    }
                }

                match shared.lock() {
                    Ok(s) => {
                        for message in s.ringbuf.iter() {
                            if let Message::Event(e) = message {
                                if e.method == "Page.frameNavigated" && e.params.frame.id == frame_id && e.params.frame.loader_id == loader_id {
                                    return Ok(());
                                }
                            }
                        }
                    },
                    Err(e) => return Err(string_error::into_err(format!("poisoned mutex: {}", e)))
                }

                thread::sleep(Duration::from_millis(100));
            }
        }
    }
}

impl Drop for Browser {
    fn drop(&mut self) {
        match self.execute("Browser.close", None, Some(Duration::from_secs(0))) {
            Ok(_) => {},
            Err(e) => {
                if let Some(f) = e.downcast_ref::<io::Error>() {
                    if f.kind() != io::ErrorKind::TimedOut {
                        log::error!("Browser.close error: {}", f);
                    }
                } else {
                    log::error!("Browser.close error: {}", e);
                }
            }
        }

        let mut alive = true;
        let start = Instant::now();
        loop {
            match self.child.try_wait() {
                Ok(Some(_)) => {
                    alive = false;
                    break;
                },
                Ok(None) => {},
                Err(e) => {
                    log::error!("process try_wait error: {}", e);
                    break;
                }
            }

            if Instant::now().duration_since(start) > Duration::from_secs(15) {
                break;
            }
            thread::sleep(Duration::from_millis(100));
        }


        if !alive {
            return;
        }
        log::warn!("deadline reached waiting for Browser.close");

        match kill(Pid::from_raw(self.child.id() as i32), Signal::SIGTERM) {
            Ok(()) => {
                for _ in 0..15 {
                    match self.child.try_wait() {
                        Ok(Some(_)) => {
                            alive = false;
                            break;
                        },
                        Ok(None) => {},
                        Err(e) => {
                            log::error!("process try_wait error: {}", e);
                            break;
                        }
                    }
                    thread::sleep(Duration::from_secs(1));
                }
            },
            Err(e) => log::error!("error sending SIGTERM: {}", e)
        }

        if !alive {
            return;
        }
        log::warn!("deadline reached waiting for SIGTERM");

        if let Err(e) = self.child.kill() {
            log::error!("process kill error: {}", e);
        }
    }
}

impl Debug for Browser {
    fn fmt(&self, f: &mut Formatter<'_>) -> fmt::Result {
        write!(f, "Browser {{ pid: {} }}", self.child.id())
    }
}

impl BrowserShared {
    fn send_request(&mut self, request: &str) -> Result<(), Box<dyn Error>> {
        match self.websocket.write_message(protocol::Message::Text(request.to_string())) {
            Ok(_) => Ok(()),
            Err(e) => Err(Box::new(e))
        }
    }
}

struct RingBuf<T> {
    buffer: Vec<T>,
    write_idx: usize,
}

struct RingBufIter<'a, T> {
    ringbuf: &'a RingBuf<T>,
    stop_idx: usize,
    read_idx: usize,
    done: bool,
}

impl<T> RingBuf<T> {
    fn new() -> Self {
        Self {
            buffer: Vec::with_capacity(1000),
            write_idx: 0,
        }
    }

    fn push(&mut self, value: T) {
        if self.buffer.len() < self.buffer.capacity() {
            self.buffer.push(value);
        } else {
            self.buffer[self.write_idx] = value;
            self.write_idx = (self.write_idx + 1) % self.buffer.capacity();
        }
    }

    fn iter(&self) -> RingBufIter<T> {
        let (done, start_idx, stop_idx) = if self.buffer.len() == 0 {
            (true, 0, 0)
        } else if self.buffer.len() < self.buffer.capacity() {
            (false, 0, self.buffer.len())
        } else {
            (false, self.write_idx, self.write_idx)
        };

        RingBufIter{
            ringbuf: self,
            stop_idx: stop_idx,
            read_idx: start_idx,
            done: done,
        }
    }
}

impl<'a, T> Iterator for RingBufIter<'a, T> {
    type Item = &'a T;

    fn next(&mut self) -> Option<Self::Item> {
        if self.done {
            None
        } else {
            let retval = Some(&self.ringbuf.buffer[self.read_idx]);
            self.read_idx = (self.read_idx + 1) % self.ringbuf.buffer.capacity();
            if self.read_idx == self.stop_idx {
                self.done = true;
            }
            retval
        }
    }
}

// TODO Add Browser.new_incognito()
// TODO Add Browser.evalute_number(), etc as needed
//      Also consider adding Browser.evaluate (no return type) and replace use of execute() in new()
// TODO Add support for thread-safe, simultaneous sessions
