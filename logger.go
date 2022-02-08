package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	traceLevel = iota
	debugLevel
	infoLevel
	warningLevel
	errorLevel
	panicLevel
)

var logLevels = map[int]string{
	  traceLevel: "trace",
	  debugLevel: "debug",
	   infoLevel: "info",
	warningLevel: "warning",
	  errorLevel: "error",
	  panicLevel: "panic",
}

type logger struct {
	Level   int
	Writers []io.Writer
	mux     sync.Mutex
}

var defaultLogger = logger{
	  Level: debugLevel,
	Writers: []io.Writer{os.Stderr},
}

func (l *logger) log(level int, format string, values ...interface{}) string {
	l.mux.Lock()
	defer l.mux.Unlock()

	var builder strings.Builder

	now := time.Now()
	builder.WriteString(now.Format("2006/01/02 15:04:05") + fmt.Sprintf(".%03d", now.Nanosecond() / 1000000))

	builder.WriteString(" " + logLevels[level])

	_, file, line, ok := runtime.Caller(2)
	if !ok {
		panic("runtime.Caller() failed")
	}
	builder.WriteString(fmt.Sprintf(" %s:%d", path.Base(file), line))

	if _, err := fmt.Fprintf(&builder, ": " + format, values...); err != nil {
		panic(err)
	}

	entry := builder.String()
	if !strings.HasSuffix(entry, "\n") {
		entry += "\n"
	}

	for _, w := range l.Writers {
		if _, err := w.Write([]byte(entry)); err != nil {
			panic(err)
		}
	}
	
	return entry
}

func TraceLog(format string, values ...interface{}) {
	if defaultLogger.Level <= traceLevel {
		defaultLogger.log(traceLevel, format, values...)
	}
}

func DebugLog(format string, values ...interface{}) {
	if defaultLogger.Level <= debugLevel {
		defaultLogger.log(debugLevel, format, values...)
	}
}

func InfoLog(format string, values ...interface{}) {
	if defaultLogger.Level <= infoLevel {
		defaultLogger.log(infoLevel, format, values...)
	}
}

func WarningLog(format string, values ...interface{}) {
	if defaultLogger.Level <= warningLevel {
		defaultLogger.log(warningLevel, format, values...)
	}
}

func ErrorLog(obj interface{}, values ...interface{}) {
	if defaultLogger.Level <= errorLevel {
		if format, ok := obj.(string); ok {
			defaultLogger.log(errorLevel, format, values...)
		} else if err, ok := obj.(error); ok {
			defaultLogger.log(errorLevel, err.Error())
		} else {
			panic("unsupported argument type")
		}
	}
}

func PanicLog(obj interface{}, values ...interface{}) {
	var entry string
	if format, ok := obj.(string); ok {
		entry = defaultLogger.log(panicLevel, format, values...)
	} else if err, ok := obj.(error); ok {
		entry = defaultLogger.log(panicLevel, err.Error())
	} else {
		panic("unsupported argument type")
	}
	panic(entry)
}
