package deepseek

// ChatChunk is one increment of a completion stream.
type ChatChunk struct {
	Content          string
	ReasoningContent string
	MessageID        string
	SessionID        string
	FinishReason     string
}

// ModelConfig describes one selectable web model.
type ModelConfig struct {
	ID        string
	ModelType string // "default" or "expert"
	Thinking  bool
	Search    bool
}

// Model registry. IDs match what the DeepSeek web client offers; unknown
// values fall back to the plain chat model rather than failing a turn.
var modelConfigs = []ModelConfig{
	{ID: "deepseek-web", ModelType: "default"},
	{ID: "deepseek-chat", ModelType: "default"},
	{ID: "deepseek-reasoner", ModelType: "default", Thinking: true},
	{ID: "deepseek-r1", ModelType: "default", Thinking: true},
	{ID: "deepseek-chat-search", ModelType: "default", Search: true},
	{ID: "deepseek-reasoner-search", ModelType: "default", Thinking: true, Search: true},
	{ID: "deepseek-expert", ModelType: "expert"},
	{ID: "deepseek-v4-pro", ModelType: "expert", Thinking: true},
}

// Models returns the registry in declaration order.
func Models() []ModelConfig {
	out := make([]ModelConfig, len(modelConfigs))
	copy(out, modelConfigs)
	return out
}

// ResolveModel maps a request model name to a web model configuration.
func ResolveModel(id string) ModelConfig {
	for _, m := range modelConfigs {
		if m.ID == id {
			return m
		}
	}
	return ModelConfig{ID: "deepseek-web", ModelType: "default"}
}
