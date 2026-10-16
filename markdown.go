package main

import (
	"fmt"
	"html"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func wantsMarkdown(r *http.Request) bool {
	agent := strings.ToLower(strings.TrimSpace(r.UserAgent()))
	if agent == "curl" || strings.HasPrefix(agent, "curl/") {
		return true
	}
	// Wildcards alone do not opt into Markdown. Exact media types override
	// wildcard qualities, including an explicit q=0 rejection.
	type preference struct {
		quality     float64
		specificity int
	}
	markdown, htmlType := preference{specificity: -1}, preference{specificity: -1}
	explicitMarkdown := false
	for _, header := range r.Header.Values("Accept") {
		for _, part := range strings.Split(header, ",") {
			media, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			quality := 1.0
			if raw, ok := params["q"]; ok {
				quality, err = strconv.ParseFloat(raw, 64)
				if err != nil || !(quality >= 0 && quality <= 1) {
					continue
				}
			}
			media = strings.ToLower(media)
			match := func(pref *preference, specific int) {
				if specific > pref.specificity || (specific == pref.specificity && quality > pref.quality) {
					pref.specificity, pref.quality = specific, quality
				}
			}
			switch media {
			case "text/markdown":
				explicitMarkdown = true
				match(&markdown, 2)
			case "text/html":
				match(&htmlType, 2)
			case "text/*":
				match(&markdown, 1)
				match(&htmlType, 1)
			case "*/*":
				match(&markdown, 0)
				match(&htmlType, 0)
			}
		}
	}
	return explicitMarkdown && markdown.quality > 0 && markdown.quality >= htmlType.quality
}

// Escape filenames as data: no table rows, links, raw HTML or headings can be
// introduced by filesystem names. Preserve Unicode, replacing line controls.
func markdownText(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, text)
	var out strings.Builder
	for _, r := range text {
		if strings.ContainsRune("\\`*_[](){}#!|~", r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return html.EscapeString(out.String())
}

func listingURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil || r.URL.Scheme == "https" || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: path}).String()
}

func curlUploadExample(r *http.Request, path string) string {
	target := listingURL(r, path+"file.txt")
	return "curl -T file.txt '" + strings.ReplaceAll(target, "'", "'\"'\"'") + "'"
}

func writeMarkdownDirectory(w http.ResponseWriter, r *http.Request, path, parent string, files []FileInfo) {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\nDirectory: %s\n\n", markdownText(title), markdownText(path))
	if path != "/" {
		fmt.Fprintf(&out, "[Parent directory](<%s>)\n\n", listingURL(r, parent))
	}
	if len(files) == 0 {
		out.WriteString("This directory is empty.\n")
	} else {
		out.WriteString("| Name | Type | Size | Modified (UTC) | Expires (UTC) |\n")
		out.WriteString("| --- | --- | --- | --- | --- |\n")
		for _, file := range files {
			kind, name, target := "file", file.Name, path+file.Name
			if file.IsDir {
				kind, name, target = "directory", name+"/", target+"/"
			}
			expires := "—"
			if file.ExpiresUnix > 0 {
				expires = time.Unix(file.ExpiresUnix, 0).UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&out, "| [%s](<%s>) | %s | %s | %s | %s |\n",
				markdownText(name), listingURL(r, target), kind, file.Size,
				time.Unix(file.ModUnix, 0).UTC().Format(time.RFC3339), expires)
		}
	}
	if enableUpload {
		fmt.Fprintf(&out, "\n## Upload a file\n\n```sh\n%s\n```\n", curlUploadExample(r, path))
	}
	body := out.String()
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodHead {
		fmt.Fprint(w, body)
	}
}
