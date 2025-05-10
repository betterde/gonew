package project

type Variable struct {
	Name        string `yaml:"name"`
	Placeholder string `yaml:"placeholder"`
}

type Config struct {
	Name               string     `yaml:"name"`
	Desc               string     `yaml:"desc"`
	Variables          []Variable `yaml:"variables"`
	DeleteTemplateFile bool       `yaml:"delete_template_file"`
}
