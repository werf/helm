package chart

import (
	"text/template"

	"helm.sh/helm/v3/pkg/cli"
)

type ChartExtenderBufferedFile struct {
	Name string
	Data []byte
}

type ChartExtender interface {
	ChartCreated(c *Chart) error
	ChartLoaded(files []*ChartExtenderBufferedFile) error
	ChartDependenciesLoaded() error
	MakeValues(inputVals map[string]interface{}) (map[string]interface{}, error)
	SetupTemplateFuncs(t *template.Template, funcMap template.FuncMap)

	LoadDir(dir string) (bool, []*ChartExtenderBufferedFile, error)
	LocateChart(name string, settings *cli.EnvSettings) (bool, string, error)
	ReadFile(filePath string) (bool, []byte, error)
}
