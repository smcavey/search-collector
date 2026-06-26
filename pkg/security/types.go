// Copyright Contributors to the Open Cluster Management project

package security

// SecurityConfig is the top-level configuration for the security scanner.
type SecurityConfig struct {
	Enabled bool    `yaml:"enabled" json:"enabled"`
	Checks  []Check `yaml:"checks" json:"checks"`
}

// Check defines a single security check with match criteria and evaluation rules.
type Check struct {
	Name        string   `yaml:"name" json:"name"`
	Severity    string   `yaml:"severity" json:"severity"` // critical, high, medium, low
	Description string   `yaml:"description" json:"description"`
	Match       Match    `yaml:"match" json:"match"`
	Rules       []Rule   `yaml:"rules" json:"rules"`
}

// Match specifies which resources a check applies to.
type Match struct {
	Kinds     []string `yaml:"kinds" json:"kinds"`
	APIGroups []string `yaml:"apiGroups" json:"apiGroups"`
}

// Rule defines a single evaluation condition within a check.
// All rules in a check must match (AND logic).
// Multiple paths within a rule use OR logic (any path match fires).
type Rule struct {
	Paths    []string    `yaml:"paths" json:"paths"`
	Operator string      `yaml:"operator" json:"operator"`
	Value    interface{} `yaml:"value" json:"value"`
}

// Finding represents a single security finding emitted by the scanner.
type Finding struct {
	Check       string
	Severity    string
	Description string
	Kind        string
	Name        string
	Namespace   string
	Detail      string
}
