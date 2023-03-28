package types

type FlowChild struct {
	ArgsMap  map[string]string `json:"args_map"`
	Function string            `json:"function"`
}

type Args struct {
	Main []string `json:"main"`
}

type Flow struct {
	Args     Args                 `json:"args"`
	Children map[string]FlowChild `json:"children,omitempty"`
}

type Flows struct {
	Flows map[string]Flow `json:"flows"`
}

type FlowOutput struct {
	Data     map[string]interface{} `json:"data"`
	Function string                 `json:"function"`
}

type FlowInput struct {
	Args     map[string]interface{} `json:"args"`
	Children map[string]*FlowOutput `json:"children"`
}
