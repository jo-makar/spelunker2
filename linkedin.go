package main

import (
	"net/url"
	"time"
)

type Job struct {
}

func LinkedIn(browser *Browser, options map[string]interface{}) chan Job {
	c := make(chan Job)

	go func() {
		defer close(c)

		remote := true
		if r, ok := options["remote"].(bool); ok {
			remote = r
		}

		updateQuery := func(u, p, v string) string {
			o, _ := url.Parse(u)
			q := o.Query()
			q.Set(p, v)
			o.RawQuery = q.Encode()
			return o.String()

		}

		baseUrl := "https://linkedin.com/jobs/search/"
		if remote {
			baseUrl = updateQuery(baseUrl, "f_WT", "2")
		}

		// FIXME stopped

		if timedout, err := browser.Goto(baseUrl, nil); err != nil {
			ErrorLog(err)
			return
		} else if timedout {
			ErrorLog("timed out")
			return
		}

		if count, err := browser.EvaluateInt("document.querySelectorAll('[data-job-id]').length", nil); err != nil {
			ErrorLog(err)
			return
		} else {
			InfoLog("count = %d", count)
		}

		if err := browser.Evaluate("window.scrollTo(0, window.scrollY + window.innerHeight)", nil); err != nil {
			ErrorLog(err)
			return
		}

		time.Sleep(3 * time.Second)

		if count, err := browser.EvaluateInt("document.querySelectorAll('[data-job-id]').length", nil); err != nil {
			ErrorLog(err)
			return
		} else {
			InfoLog("count = %d", count)
		}
	}()

	return c
}
