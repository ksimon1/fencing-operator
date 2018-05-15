package stub

import (
	"fmt"

	"github.com/beekhof/fencing-operator/pkg/apis/fencing/v1alpha1"

	"github.com/operator-framework/operator-sdk/pkg/sdk/action"
	"github.com/operator-framework/operator-sdk/pkg/sdk/query"
	"github.com/operator-framework/operator-sdk/pkg/sdk/handler"
	"github.com/operator-framework/operator-sdk/pkg/sdk/types"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"

//	"github.com/operator-framework/operator-sdk/pkg/k8sclient"
//	"k8s.io/apimachinery/pkg/fields"
)

var (
	// TODO read supported source from node problem detector config - this issue is still WIP
	supportedNodeProblemSources = sets.NewString("abrt-notification", "abrt-adaptor", "docker-monitor", "kernel-monitor", "kernel")
)


func NewHandler() handler.Handler {
	return &Handler{}
}

type Handler struct {
	// Fill me
}

func (h *Handler) Handle(ctx types.Context, event types.Event) error {
	switch o := event.Object.(type) {
	case *v1.Node:
		h.HandleNode(ctx, o, event.Deleted)
		
	case *v1.Event:
		h.HandleEvent(ctx, o, event.Deleted)
		
	case *v1.ConfigMap:
		h.HandleConfigMap(ctx, o, event.Deleted)

	case *v1alpha1.FencingRequest:
		h.HandleFencingRequest(ctx, o, event.Deleted)
	}
	return nil
}

func (h *Handler) HandleNode(ctx types.Context, node *v1.Node, deleted bool) error {
	if deleted {
		logrus.Errorf("Node deleted : %v ", node)
		h.CancelFencingRequest(node, nil)
		
	} else  {
		for _, condition := range node.Status.Conditions {
			if h.isNodeDirty(node, condition) {
				h.CreateFencingRequest(node, fmt.Sprintf("%v", condition))
				return nil // No need to continue processing
			}
		}
	}
	return nil
}

func (h *Handler) HandleEvent(ctx types.Context, event *v1.Event, deleted bool) error {
	if event.Type != v1.EventTypeWarning || !supportedNodeProblemSources.Has(string(event.Source.Component)) {
		return nil
	}

	node := &v1.Node{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Node",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      event.Source.Host,
		},
	}
	
	err := query.Get(node)
	if err != nil {
		logrus.Errorf("Failed to get node '%s': %s", event.Source.Host, err)
		return err
	}
	h.CreateFencingRequest(node, event.Reeason)
	return nil
}

func (h *Handler) dirtyVolumesForNode(node v1.Node) []v1.Volume {
	// Might be interesting to implement/use at some point
	return []v1.Volume{}
}

func (h *Handler) listPods(node *v1.Node) (error, []v1.Pod) {
	pods := &v1.PodList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
	}

	sel := fmt.Sprintf("spec.nodeName=%s", node.Name)
	opt := &metav1.ListOptions{FieldSelector: sel}
	err := query.List("--all-namespaces", pods, query.WithListOptions(opt))
	if err != nil {
		logrus.Errorf("failed to get pod list: %v", err)
	}
	return err, pods.Items
}

func (h *Handler) dirtyVolumes(pod v1.Pod) []v1.Volume {
	volumes := []v1.Volume{}
	for _, vol := range pod.Spec.Volumes {
		if vol.VolumeSource.PersistentVolumeClaim != nil {
			volumes = append(volumes, vol)
		}
	}
	return volumes
}

func (h *Handler) isPodDirty(pod v1.Pod) bool {
	dirty := false
	volumes := h.dirtyVolumes(pod)

	if len(volumes) > 0 {
		dirty = true
		logrus.Infof("Pod: %s/%s on %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
		for _, vol := range volumes {
			claim := vol.VolumeSource.PersistentVolumeClaim.ClaimName
			logrus.Infof("\tpvc: %v", claim)


			pvc := &v1.PersistentVolumeClaim{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolumeClaim",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      vol.VolumeSource.PersistentVolumeClaim.ClaimName,
				},
			}

			err := query.Get(pvc)
			if err != nil {
				logrus.Errorf("failed to get persistent volume claim: %v", err)
			} else {
				// if len(pvc.Spec.VolumeName) != 0 {
				logrus.Infof("\tpv: %v", pvc.Spec.VolumeName)
				// pv, err := c.client.CoreV1().PersistentVolumes().Get(pvc.Spec.VolumeName, metav1.GetOptions{})
			}
		}
	}
	return dirty
}

func (h *Handler) isNodeDirty(node *v1.Node, condition v1.NodeCondition) bool {
	dirty := false
	if v1.NodeReady == condition.Type && v1.ConditionUnknown == condition.Status {
		// https://kubernetes.io/docs/concepts/architecture/nodes/#condition
		_, pods := h.listPods(node)
		for _, pod := range pods {
			if h.isPodDirty(pod) {
				dirty = true
			}
		}
	}
	if dirty {
		logrus.Warningf("Node %s is lost with attached persistent volumes", node.Name)
	} else {
		logrus.Warningf("Node %s is lost", node.Name)
	}
	return dirty
}

func (h *Handler) listFencingRequests(node *v1.Node, name string) (error, []v1.FencingRequest) {
	requestList := &v1alpha.FencingRequestList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "FencingRequest",
			APIVersion: "v1alpha1",
		},
	}

	var sel
	if len(name) > 0 {
		sel = fmt.Sprintf("spec.target=%s,name=%s", node.Name, name)
	} else {
		sel = fmt.Sprintf("spec.target=%s", node.Name)
	}
	sel := fmt.Sprintf("spec.target=%s", node.Name)
	opt := &metav1.ListOptions{FieldSelector: sel}
	err := query.List("--all-namespaces", pods, query.WithListOptions(opt))
	if err != nil {
		logrus.Errorf("Failed to get fencing request list: %v", err)
	}
	return err, requestList.Items
}

func (h *Handler) DeleteFencingRequest(node *v1.Node, name string) error {
	// Delete a specific request or all for the supplied node
	_, requests := listFencingRequests(node, name) 
	for _, request := range requests {
		err := action.Delete(request)
		if err != nil {
			logrus.Errorf("Failed to delete fencing request %v: %v", request.UID, err)
		}
	}
}

func (h *Handler) CreateFencingRequest(node *v1.Node, cause string) error {

	// Look for any existing FencingRequests, only create a new one if not found
	_, requests := listFencingRequests(node, name) 
	for _, request := range requests {
		if request.Status.Complete {
			logrus.Infof("Node %s is already scheduled for fencing by %v", node.UID, request.Name)
			return nil
		}
	}
	
	backoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   1.2,
		Steps:    5,
	}

	request := newFencingRequest(node, cause)

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		err := action.Create(request)
		if err != nil && !errors.IsAlreadyExists(err) {
			// Re-Try it as errors writing to the API server are common
			return false, err
		}
		return true, nil
	})
	
	if err != nil {
		logrus.Errorf("Failed to create fencing request for node %s: %v", node.Name, err)
	} else {
		logrus.Infof("Created fencing request for node %s: %s", node.Name, cause)
	}
	return err
}


func newFencingRequest(node *v1.Node, cause string) *v1alpha1.FencingRequest {
	name := fmt.Sprintf("node-fence-%s-", node.Name)
	labels := map[string]string{
		"app": "busy-box",
	}
	// volumes := h.dirtyVolumes(pod) // Do anything with these perhaps?  Disk fencing?
	return &v1alpha1.FencingRequest{
		TypeMeta: metav1.TypeMeta{
			Kind:       "FencingRequest",
			APIVersion: "v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name,
			Namespace:    "default",
			Labels: labels,
/* TODO: Link to the operator itself
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(cr, schema.GroupVersionKind{
					Group:   v1alpha1.SchemeGroupVersion.Group,
					Version: v1alpha1.SchemeGroupVersion.Version,
					Kind:    "FencingSet",
					UID:     "FencingSet",
				}),
			},
*/
		},
		Spec: v1alpha1.FencingRequestSpec{
			Target: node.Name,
			Origin: cause,
			Operation: "Off",
			//ValidAfter date.Time `json:"validAfter,omitempty"`
		},
	}
}
