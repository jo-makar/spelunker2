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
use std::time::Duration;

use timeout_readwrite::TimeoutReader;

use tungstenite::{self, protocol::{self, WebSocket}};

use url::Url;

struct BrowserShared {
    websocket: WebSocket<TcpStream>,
}

pub struct Browser {
    child: Child,
    shared: Arc<Mutex<BrowserShared>>,
    request_id: u64,
}

struct Response {
    // FIXME
}

struct Event {
    // FIXME
}

enum Message {
    Response(Response),
    Event(Event),
}

impl Browser {
    pub fn new() -> Result<Self, Box<dyn Error>> {
        let mut cmd = Command::new("/opt/google/chrome/chrome");
        cmd.args(&["--headless", "--remote-debugging-port=0"]);

        match env::var("HOME") {
            Ok(h) => { cmd.arg(format!("--user-data-dir={}/.config/google-chrome", h)); },
            Err(e) => {
                log::error!("error to determining HOME: {}", e);
                return Err(Box::new(e));
            }
        }

        cmd.stdin(Stdio::null())
           .stdout(Stdio::null())
           .stderr(Stdio::piped());

        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                log::error!("error launching chrome: {}", e);
                return Err(Box::new(e));
            }
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
                let s = "websocket url not found";
                log::error!("{}", s);
                let _ = child.kill();
                return Err(string_error::static_err(s));
            }
        }
        let port = port.unwrap();

        let debugger_url: String;
        {
            #[derive(Deserialize, Debug)]
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
                        Ok(c) => {
                            let s = format!("unexpected json url response config count ({})", c.len());
                            log::error!("{}", s);
                            let _ = child.kill();
                            return Err(string_error::into_err(s));
                        },
                        Err(e) => {
                            log::error!("error parsing json url response: {}", e);
                            let _ = child.kill();
                            return Err(Box::new(e));
                        }
                    }
                },
                Ok(r) => {
                    let s = format!("{} status from json url", r.status());
                    log::error!("{}", s);
                    let _ = child.kill();
                    return Err(string_error::into_err(s));
                },
                Err(e) => {
                    log::error!("error accessing json url: {}", e);
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
                    log::error!("error parsing debbuger url: {}", e);
                    let _ = child.kill();
                    return Err(Box::new(e));
                }
            };

            stream = match TcpStream::connect(format!("127.0.0.1:{}", port)) {
                Ok(s) => s,
                Err(e) => {
                    log::error!("error connecting to debbuger url: {}", e);
                    let _ = child.kill();
                    return Err(Box::new(e));
                }
            };
        }

        let mut websocket: WebSocket<TcpStream>;
        match tungstenite::client::client(debugger_url, stream) {
            Ok((w, r)) if r.status() == StatusCode::SWITCHING_PROTOCOLS => websocket = w,
            Ok((_, r)) => {
                let s = format!("{} status from debugger url", r.status());
                log::error!("{}", s);
                let _ = child.kill();
                return Err(string_error::into_err(s));
            },
            Err(e) => {
                log::error!("error connecting to websocket: {}", e);
                let _ = child.kill();
                return Err(Box::new(e));
            }
        }

        if let Err(e) = websocket.get_mut().set_nonblocking(true) {
            log::error!("set_nonblocking error: {}", e);
            let _ = child.kill();
            return Err(Box::new(e));
        }

        let shared = Arc::new(Mutex::new(BrowserShared{
            websocket: websocket,
        }));

        {
            let shared = shared.clone();
            thread::spawn(move || {
                loop {
                    match shared.lock() {
                        Ok(ref mut s) => {
                            match s.websocket.read_message() {
                                Ok(m) => {
                                    // FIXME Store messages (responses/events enum) in a circular buffer
                                    log::info!("websocket message: {}", m);
                                },
                                Err(tungstenite::error::Error::Io(e)) if e.kind() == io::ErrorKind::WouldBlock => {},
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

        // FIXME STOPPED
        {
            let mut params: Map<String, Value> = Map::new();
            params.insert("expression".to_string(), Value::String("navigator.userAgent".to_string()));
            let _ = retval.execute("Runtime.evaluate", Some(params));
        }

        Ok(retval)
    }

    // FIXME Include timeout here, including forever and immediate return
    fn execute(&mut self, method: &str, param: Option<Map<String, Value>>) -> Result<Response, Box<dyn Error>> {
        {
            #[derive(Serialize)]
            struct Request {
                id: Number,
                method: String,
                #[serde(skip_serializing_if = "Option::is_none")]
                param: Option<Map<String, Value>>,
            }

            let request = Request{
                id: Number::from(self.request_id),
                method: method.to_string(),
                param: param,
            };
            self.request_id += 1;

            let request_string = match serde_json::to_string(&request) {
                Ok(s) => s,
                Err(e) => {
                    log::error!("json serialization error: {}", e);
                    return Err(Box::new(e));
                }
            };
            log::trace!(">>> {}", request_string);

            match self.shared.clone().lock() {
                Ok(mut s) => {
                    match s.send_request(&request_string) {
                        Ok(_) => {},
                        Err(e) => {
                            log::error!("error sending message: {}", e);
                            return Err(e);
                        }
                    }
                },
                Err(e) => {
                    let s = format!("poisoned mutex: {}", e);
                    log::error!("{}", s);
                    return Err(string_error::into_err(s));
                }
            }
        }

        // FIXME STOPPED Loop for optional timeout while thread collects potential responses

        Ok(Response {})
    }
}

impl Drop for Browser {
    fn drop(&mut self) {
        let mut alive = true;
        // FIXME Send a close command and test if alive with 15 sec timeout (as below)
        //       Generalize the test-if-alive code below in an internal function

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

// TODO Add support for thread-safe, simultaneous sessions
