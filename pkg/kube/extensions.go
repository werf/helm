package kube

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *Client) DeleteNamespace(ctx context.Context, namespace string, opts DeleteOptions) error {
	cs, err := c.Factory.KubernetesClientSet()
	if err != nil {
		return err
	}

	if err := cs.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{}); err != nil {
		return err
	}

	if opts.Wait {
		specs := []*ResourcesWaiterDeleteResourceSpec{
			{ResourceName: namespace, Namespace: "", GroupVersionResource: schema.GroupVersionResource{
				Group: "v1",
			}},
		}
		if err := c.ResourcesWaiter.WaitUntilDeleted(context.Background(), ResourcesWaiterDeleteResourceSpec{ResourceName: namespace, Namespace: ""}, opts.WaitTimeout); err != nil {
			return fmt.Errorf("waiting until namespace deleted failed: %s", err)
		}
	}

	return nil
}
