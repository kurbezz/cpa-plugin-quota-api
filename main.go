package main

const pluginName = "quota-api"

var pluginVersion = "0.1.0"
var claudeUsageEndpoint = "https://api.anthropic.com/api/oauth/usage"
var codexUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"
var activeRuntime = newPluginRuntime(cgoHostClient{})

func main() {}
