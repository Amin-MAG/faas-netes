package types

type FlowChild struct {
	ArgsMap  map[string]string `json:"args_map"`
	Function string            `json:"function"`
}

type Flow struct {
	Args          []string             `json:"args"`
	Children      map[string]FlowChild `json:"children,omitempty"`
	Caching       bool                 `json:"caching"`
	CacheTTL      uint                 `json:"cache_ttl"`
	IsThirdParty  bool                 `json:"is_third_party"`
	ThirdPartyURL *string              `json:"third_party_url"`
}

type Flows struct {
	Flows map[string]Flow `json:"flows"`
}

type FlowOutput struct {
	Data     []byte `json:"data"`
	Function string `json:"function"`
}

type FlowInput struct {
	Args     map[string]interface{} `json:"args"`
	Children map[string]*FlowOutput `json:"children"`
}
