package chart

type ChartExtender interface {
	SetupChart(c *Chart) error
	AfterLoad() error
	MakeValues(inputVals map[string]interface{}) (map[string]interface{}, error)
}
