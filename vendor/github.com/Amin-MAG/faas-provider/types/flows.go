package types

type FlowChild struct {
	Args     []string `json:"args"`
	Function string   `json:"function"`
}

type Flow struct {
	Args     []string             `json:"args"`
	Children map[string]FlowChild `json:"children"`
}

type Flows struct {
	Flows map[string]Flow `json:"flows"`
}
