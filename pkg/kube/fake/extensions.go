package fake

import (
	"context"

	"helm.sh/helm/v3/pkg/kube"
)

func (c *PrintingKubeClient) DeleteNamespace(ctx context.Context, namespace string, opts kube.DeleteOptions) error {
	return nil
}
