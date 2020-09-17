package chart

import "text/template"

type ChartExtender interface {
	SetupChart(c *Chart) error
	AfterLoad() error
	MakeValues(inputVals map[string]interface{}) (map[string]interface{}, error)
	SetupTemplateFuncs(t *template.Template, funcMap template.FuncMap)
}
