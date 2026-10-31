module github.com/bojackduy/deepseek-proxy

go 1.27.2

require golang.org/x/text v0.14.0 // indirect

require (
	github.com/bojackduy/bridgecore v0.0.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
)

replace github.com/bojackduy/bridgecore => ../bridgecore
