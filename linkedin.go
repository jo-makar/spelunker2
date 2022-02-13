package main

import (
	"github.com/PuerkitoBio/goquery"

	"bytes"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Job struct {
	Html     string
	Url      string
	Title    string
	Company  string
	Location string
	Remote   bool
	Metadata []string
}

func LinkedIn(browser *Browser, options map[string]interface{}) chan Job {
	c := make(chan Job)

	go func() {
		defer close(c)

		remote := true
		if r, ok := options["remote"].(bool); ok {
			remote = r
		}

		updateQuery := func(u, p string, v interface{}) string {
			o, _ := url.Parse(u)
			q := o.Query()
			q.Set(p, fmt.Sprintf("%v", v))
			o.RawQuery = q.Encode()
			return o.String()

		}

		baseUrl := "https://linkedin.com/jobs/search/"
		if remote {
			baseUrl = updateQuery(baseUrl, "f_WT", "2")
		}

		for start := 0; start <= 500; start += 25 {
			pageUrl := baseUrl
			if start > 0 {
				pageUrl = updateQuery(pageUrl, "start", start)
			}
			InfoLog("processing %s", pageUrl)

			if timedout, err := browser.Goto(pageUrl, nil); err != nil {
				ErrorLog(err)
				return
			} else if timedout {
				ErrorLog("timed out loading %s", pageUrl)
				return
			}

			//
			// Scroll through the page to populate the jobs
			//

			stop := time.Now().Add(100 * time.Second)
			for {
				var counts []int
				for i := 0; i < 3; i++ {
					if count, err := browser.EvaluateInt("document.querySelectorAll('[data-job-id]').length", nil); err != nil {
						ErrorLog(err)
						return
					} else {
						counts = append(counts, count)
					}

					time.Sleep(1 * time.Second)
				}
				DebugLog("(job) counts = %v", counts)

				if counts[len(counts)-1] >= 25 && counts[0] == counts[len(counts)-1] {
					break
				}

				if counts[0] == counts[len(counts)-1] {
					if err := browser.Evaluate("window.scrollTo(0, window.scrollY + window.innerHeight)", nil); err != nil {
						ErrorLog(err)
						return
					}
				}

				if time.Now().After(stop) {
					ErrorLog("timed out scrolling through jobs")
					return
				}
			}

			//
			// Parse the displayed jobs
			//

			html, err := browser.EvaluateString("document.documentElement.outerHTML", nil)
			if err != nil {
				ErrorLog(err)
				return
			}

			if jobs, err := parseJobs(html); err != nil {
				ErrorLog(err)
				return
			} else if len(jobs) == 0 {
				ErrorLog("no jobs successfully parsed")
				return
			} else {
				for _, job := range jobs {
					c <- job
				}
			}
		}

		InfoLog("exceeded max linkedin page views")
	}()

	return c
}

func parseJobs(html string) ([]Job, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}

	jobs := []Job{}
	doc.Find("[data-job-id]").Each(func(i int, s *goquery.Selection) {
		var job Job

		if html, err := goquery.OuterHtml(s); err != nil {
			ErrorLog("%d: OuterHtml: %s", i, err.Error())
			return
		} else {
			job.Html = html
		}

		if idstr, exists := s.Attr("data-job-id"); !exists {
			ErrorLog("\"data-job-id\" doesn't exist")
			return
		} else {
			if idnum, err := strconv.Atoi(idstr); err != nil {
				ErrorLog("%d: strconv(id): %s", i, err.Error())
				return
			} else {
				job.Url = fmt.Sprintf("https://linkedin.com/jobs/view/%d", idnum)
			}
		}

		job.Title = strings.TrimSpace(s.Find(".job-card-list__title").Text())
		if job.Title == "" {
			ErrorLog("%d: missing title", i)
			return
		}

		job.Company = strings.TrimSpace(s.Find(".job-card-container__company-name").Text())
		if job.Company == "" {
			ErrorLog("%d: missing company", i)
			return
		}

		s.Find(".job-card-container__metadata-item").Each(func(j int, t *goquery.Selection) {
			if j == 0 {
				job.Location = strings.TrimSpace(t.Text())
			} else {
				if j == 1 && strings.ToLower(strings.TrimSpace(t.Text())) == "remote" {
					job.Remote = true
				} else {
					job.Metadata = append(job.Metadata, strings.TrimSpace(t.Text()))
				}
			}
		})

		jobs = append(jobs, job)
	})

	return jobs, nil
}

func (j *Job) Write(w io.Writer) (int, error) {
	var b bytes.Buffer

	b.WriteString("<table><tr>")

	// TODO Svg images are not support by Gmail, investigate workarounds

	// `<svg xmlns="http://www.w3.org/2000/svg" width="34" height="34" viewBox="0 0 34 34">
        //    <g><path d="M34,2.5v29A2.5,2.5,0,0,1,31.5,34H2.5A2.5,2.5,0,0,1,0,31.5V2.5A2.5,2.5,0,0,1,2.5,0h29A2.5,2.5,0,0,1,34,2.5ZM10,13H5V29h5Zm.45-5.5A2.88,2.88,0,0,0,7.59,4.6H7.5a2.9,2.9,0,0,0,0,5.8h0a2.88,2.88,0,0,0,2.95-2.81ZM29,19.28c0-4.81-3.06-6.68-6.1-6.68a5.7,5.7,0,0,0-5.06,2.58H17.7V13H13V29h5V20.49a3.32,3.32,0,0,1,3-3.58h.19c1.59,0,2.77,1,2.77,3.52V29h5Z" fill="#0a66c2"></path></g>
	// </svg>

	//b.WriteString(`<td><img src="https://content.linkedin.com/content/dam/me/about/LinkedIn_Icon.jpg.original.jpg"/></td>`)

	b.WriteString("<td>")

	b.WriteString(fmt.Sprintf(`<div><a href="%s">%s</a></div>`, j.Url, j.Title))
	b.WriteString("<div>" + j.Company + " - " + j.Location + "</div>")
	b.WriteString("<div>" + strings.Join(j.Metadata, ", ") + "</div>")

	b.WriteString("</td>")
	b.WriteString("</tr></table>\n")

	return w.Write(b.Bytes())
}
