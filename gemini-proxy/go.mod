module github.com/bojackduy/gemini-proxy

go 1.27.2

require (
	github.com/chromedp/sysutil v1.1.0 // indirect
	github.com/go-json-experiment/json v0.0.0-20260623181947-01eb4420fa68 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.4.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.14.0 // indirect
)

require (
	github.com/bojackduy/bridgecore v0.0.0-00010101000000-000000000000
	github.com/chromedp/cdproto v0.0.0-20260714215040-dc233986426f
	github.com/chromedp/chromedp v0.16.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
)

replace github.com/bojackduy/bridgecore => ../bridgecore
