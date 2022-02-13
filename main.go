package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	var jobsFlag, newsFlag, socialFlag bool

	flag.BoolVar(&jobsFlag, "jobs", false, "jobs spelunking")
	flag.BoolVar(&jobsFlag, "j", false, "jobs spelunking")

	flag.BoolVar(&newsFlag, "news", false, "news spelunking")
	flag.BoolVar(&newsFlag, "n", false, "news spelunking")

	flag.BoolVar(&socialFlag, "social", false, "social media spelunking")
	flag.BoolVar(&socialFlag, "s", false, "social media spelunking")

	flag.Parse()

	// Write log messages to buffer for inclusion in emails
	var logbuf bytes.Buffer
	defaultLogger.Writers = append(defaultLogger.Writers, &logbuf)

	//
	// Browser setup
	//

	browser, err := NewBrowser(false)
	if err != nil {
		ErrorLog(err)
		os.Exit(1)
	}
	defer browser.Close()

	//
	// Sqlite database setup
	//

	sqlite, err := NewSqlite("seen.db")
	if err != nil {
		ErrorLog(err)
		os.Exit(1)
	}
	defer sqlite.Close()

	if !sqlite.Exists {
		sql := `create table seen (url text primary key, added text not null)`
		if _, err := sqlite.Exec(sql); err != nil {
			ErrorLog(err)
			os.Exit(1)
		}
	}
	sql := `delete from seen where added < datetime('now', '-90 days')`
	if _, err := sqlite.Exec(sql); err != nil {
		ErrorLog(err)
		os.Exit(1)
	}

	sql = `select count(*) from seen where url=?`
	seenStmt, err := sqlite.Prepare(sql)
	if err != nil {
		ErrorLog(err)
		os.Exit(1)
	}
	defer seenStmt.Close()

	urlSeen := func(u string) (bool, error) {
		var count int
		err := seenStmt.QueryRow(u).Scan(&count)
		return count > 0, err
	}

	sql = `insert into seen (url, added) values (?, datetime('now'))`
	addStmt, err := sqlite.Prepare(sql)
	if err != nil {
		ErrorLog(err)
		os.Exit(1)
	}
	defer addStmt.Close()

	addUrl := func(u string) error {
		_, err := addStmt.Exec(u)
		return err
	}

	//
	// Scraping by category
	//

	if jobsFlag {
		var jobs []Job
		for job := range LinkedIn(browser, nil) {
			if seen, err := urlSeen(job.Url); err != nil {
				ErrorLog(err)
				os.Exit(1)
			} else if seen {
				DebugLog("skipping seen job: %s", job.Url)
				continue
			}

			jobs = append(jobs, job)

			if err := addUrl(job.Url); err != nil {
				ErrorLog(err)
				os.Exit(1)
			}

			if len(jobs) >= 100 {
				InfoLog("scraped targeted new job count")
				break
			}
		}

		var body bytes.Buffer

		body.WriteString("<html><body>")

		body.WriteString("<pre>" + logbuf.String() + "</pre>")

		for _, job := range jobs {
			if _, err := job.Write(&body); err != nil {
				ErrorLog(err)
				os.Exit(1)
			}
		}
		body.WriteString("</body></html>")

		subj := fmt.Sprintf("spelunker2 linkedin %s", time.Now().Format("2006-01-02"))
		if err := SendMail(body.String(), subj); err != nil {
			ErrorLog(err)
			os.Exit(1)
		}
	}

	if newsFlag {
		// TODO Scrape Hacker News, Google News, etc
		//      Be aware that muliple scrapes should be interleaved if targeting a new entry count across them
		//      Ie one goroutine for Hacker News and another for Google News and stop when total hits 100 new entries
	}

	if socialFlag {
		// TODO Scrape Twitter
	}
}
