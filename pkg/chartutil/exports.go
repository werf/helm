package chartutil

import "helm.sh/helm/v3/pkg/chart"

func CoalesceChartValues(c *chart.Chart, v map[string]interface{}) {
	coalesceValues(c, v)
}

func CoalesceChartDeps(chrt *chart.Chart, dest map[string]interface{}) (map[string]interface{}, error) {
	return coalesceDeps(chrt, dest)
}
