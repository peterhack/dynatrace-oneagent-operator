package oneagent

import (
	"context"
	"fmt"
	"reflect"
	"time"

	dynatracev1alpha1 "github.com/Dynatrace/dynatrace-oneagent-operator/pkg/apis/dynatrace/v1alpha1"
	"github.com/Dynatrace/dynatrace-oneagent-operator/pkg/controller/istio"
	"github.com/Dynatrace/dynatrace-oneagent-operator/pkg/controller/nodes"
	"github.com/Dynatrace/dynatrace-oneagent-operator/pkg/controller/utils"
	"github.com/Dynatrace/dynatrace-oneagent-operator/pkg/dtclient"

	"github.com/go-logr/logr"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// time between consecutive queries for a new pod to get ready
const splayTimeSeconds = uint16(10)

// Add creates a new OneAgent NodesController and adds it to the Manager. The Manager will set fields on the NodesController
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return NewOneAgentReconciler(mgr.GetClient(), mgr.GetScheme(), mgr.GetConfig(),
		log.Log.WithName("oneagent.controller"), utils.BuildDynatraceClient)
}

// NewOneAgentReconciler - initialise a new ReconcileOneAgent instance
func NewOneAgentReconciler(client client.Client, scheme *runtime.Scheme, config *rest.Config, logger logr.Logger,
	dynatraceClientFunc utils.DynatraceClientFunc) *ReconcileOneAgent {

	return &ReconcileOneAgent{
		client:              client,
		scheme:              scheme,
		config:              config,
		logger:              logger,
		dynatraceClientFunc: dynatraceClientFunc,
		nodesController:     nodes.NewController(client, dynatraceClientFunc),
		istioController:     istio.NewController(config),
	}
}

// add adds a new NodesController to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("oneagent-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource OneAgent
	err = c.Watch(&source.Kind{Type: &dynatracev1alpha1.OneAgent{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource DaemonSets and requeue the owner OneAgent
	err = c.Watch(&source.Kind{Type: &appsv1.DaemonSet{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &dynatracev1alpha1.OneAgent{},
	})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &corev1.Node{}}, &handler.EnqueueRequestForObject{})

	return nil
}

// ReconcileOneAgent reconciles a OneAgent object
type ReconcileOneAgent struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	config *rest.Config
	logger logr.Logger

	dynatraceClientFunc utils.DynatraceClientFunc
	nodesController     *nodes.Controller
	istioController     *istio.Controller
}

// Reconcile reads that state of the cluster for a OneAgent object and makes changes based on the state read
// and what is in the OneAgent.Spec
// Note:
// The NodesController will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileOneAgent) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	if len(request.Namespace) == 0 {
		return reconcile.Result{}, r.nodesController.ReconcileNodes(request.Name)
	}

	logger := r.logger.WithValues("namespace", request.Namespace, "name", request.Name)
	logger.Info("reconciling oneagent")

	instance := &dynatracev1alpha1.OneAgent{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not dsActual, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, err
	}
	r.scheme.Default(instance)

	if err := validate(instance); err != nil {
		return reconcile.Result{}, err
	}

	// default value for .spec.tokens
	if instance.Spec.Tokens == "" {
		instance.Spec.Tokens = instance.Name

		logger.Info("updating custom resource", "cause", "defaults applied")
		err := r.updateCR(instance)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{Requeue: true}, nil
	}

	if err := r.validateSecret(instance); err != nil {
		return reconcile.Result{}, err
	}

	dtc, err := r.dynatraceClientFunc(r.client, instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	if instance.Spec.EnableIstio {
		if upd, ok := r.istioController.ReconcileIstio(instance, dtc); ok && upd {
			return reconcile.Result{RequeueAfter: 1 * time.Minute}, nil
		}
	}

	var updateCR bool

	updateCR, err = r.reconcileRollout(logger, instance, dtc)
	if err != nil {
		return reconcile.Result{}, err
	} else if updateCR {
		logger.Info("updating custom resource", "cause", "initial rollout")
		err := r.updateCR(instance)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	if instance.Spec.DisableAgentUpdate {
		logger.Info("automatic oneagent update is disabled")
		return reconcile.Result{}, nil
	}

	updateCR, err = r.reconcileVersion(logger, instance, dtc)
	if err != nil {
		return reconcile.Result{}, err
	} else if updateCR {
		err := r.updateCR(instance)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	return reconcile.Result{RequeueAfter: 30 * time.Minute}, nil
}

func (r *ReconcileOneAgent) validateSecret(instance *dynatracev1alpha1.OneAgent) error {
	secret := &corev1.Secret{}
	err := r.client.Get(context.TODO(), client.ObjectKey{Namespace: instance.Namespace, Name: instance.Spec.Tokens}, secret)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	if errors.IsNotFound(err) {
		var message = fmt.Sprintf("The secret %s has not been found on namespace %s", instance.Spec.Tokens, instance.Namespace)
		setStatusCondition(instance, dynatracev1alpha1.ApiTokenConditionType, corev1.ConditionFalse, "TokensSecretNotFound", message)
		setStatusCondition(instance, dynatracev1alpha1.PaaSTokenConditionType, corev1.ConditionFalse, "TokensSecretNotFound", message)
	} else {
		setStatusCondition(instance, dynatracev1alpha1.ApiTokenConditionType, corev1.ConditionTrue, "", "")
		setStatusCondition(instance, dynatracev1alpha1.PaaSTokenConditionType, corev1.ConditionTrue, "", "")
	}

	return nil
}

func setStatusCondition(instance *dynatracev1alpha1.OneAgent, conditionType dynatracev1alpha1.OneAgentConditionType, conditionStatus corev1.ConditionStatus, reason string, message string) {
	condition := findCondition(instance, conditionType)
	condition.Status = conditionStatus
	condition.Reason = reason
	condition.Message = message
}

func findCondition(instance *dynatracev1alpha1.OneAgent, conditionType dynatracev1alpha1.OneAgentConditionType) *dynatracev1alpha1.OneAgentCondition {
	for i := range instance.Status.Conditions {
		if instance.Status.Conditions[i].Type == conditionType {
			return instance.Status.Conditions[i]
		}
	}

	condition := dynatracev1alpha1.OneAgentCondition{Type: conditionType}
	instance.Status.Conditions = append(instance.Status.Conditions, &condition)
	return &condition
}

func (r *ReconcileOneAgent) reconcileRollout(logger logr.Logger, instance *dynatracev1alpha1.OneAgent, dtc dtclient.Client) (bool, error) {
	updateCR := false

	// element needs to be inserted before it is used in ONEAGENT_INSTALLER_SCRIPT_URL
	if instance.Spec.Env[0].Name != "ONEAGENT_INSTALLER_TOKEN" {
		instance.Spec.Env = append(instance.Spec.Env[:0], append([]corev1.EnvVar{{
			Name: "ONEAGENT_INSTALLER_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: instance.Spec.Tokens},
					Key:                  utils.DynatracePaasToken}},
		}}, instance.Spec.Env[0:]...)...)
		updateCR = true
	}

	// Define a new DaemonSet object
	dsDesired := newDaemonSetForCR(instance)

	// Set OneAgent instance as the owner and controller
	if err := controllerutil.SetControllerReference(instance, dsDesired, r.scheme); err != nil {
		return false, err
	}

	// Check if this DaemonSet already exists
	dsActual := &appsv1.DaemonSet{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: dsDesired.Name, Namespace: dsDesired.Namespace}, dsActual)
	if err != nil && errors.IsNotFound(err) {
		logger.Info("creating new daemonset")
		err = r.client.Create(context.TODO(), dsDesired)
		if err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	} else {
		if hasSpecChanged(&dsActual.Spec, &instance.Spec) {
			logger.Info("updating existing daemonset")
			err = r.client.Update(context.TODO(), dsDesired)
			if err != nil {
				return false, err
			}
		}
	}

	if instance.Status.Version == "" {
		desired, err := dtc.GetLatestAgentVersion(dtclient.OsUnix, dtclient.InstallerTypeDefault)
		if err != nil {
			logger.Error(err, "failed to get desired version")
			return updateCR, nil
		}

		instance.Status.Version = desired
		updateCR = true
	}

	return updateCR, nil
}

func (r *ReconcileOneAgent) determineOneAgentPhase(instance *dynatracev1alpha1.OneAgent) (bool, error) {
	var phaseChanged bool
	dsActual := &appsv1.DaemonSet{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, dsActual)

	if errors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		phaseChanged = instance.Status.Phase != dynatracev1alpha1.Error
		instance.Status.Phase = dynatracev1alpha1.Error
		return phaseChanged, err
	}

	if dsActual.Status.NumberReady == dsActual.Status.CurrentNumberScheduled {
		phaseChanged = instance.Status.Phase != dynatracev1alpha1.Running
		instance.Status.Phase = dynatracev1alpha1.Running
	} else {
		phaseChanged = instance.Status.Phase != dynatracev1alpha1.Deploying
		instance.Status.Phase = dynatracev1alpha1.Deploying
	}

	return phaseChanged, nil
}

func (r *ReconcileOneAgent) reconcileVersion(logger logr.Logger, instance *dynatracev1alpha1.OneAgent, dtc dtclient.Client) (bool, error) {
	updateCR := false

	// get desired version
	desired, err := dtc.GetLatestAgentVersion(dtclient.OsUnix, dtclient.InstallerTypeDefault)
	if err != nil {
		logger.Error(err, "failed to get desired version")
		return false, nil
	} else if desired != "" && instance.Status.Version != desired {
		logger.Info("new version available", "actual", instance.Status.Version, "desired", desired)
		instance.Status.Version = desired
		updateCR = true
	}

	// query oneagent pods
	podList := &corev1.PodList{}
	listOps := []client.ListOption{
		client.InNamespace(instance.Namespace),
		client.MatchingLabels(buildLabels(instance.Name)),
	}
	err = r.client.List(context.TODO(), podList, listOps...)
	if err != nil {
		logger.Error(err, "failed to list pods", "listops", listOps)
		return updateCR, err
	}

	// determine pods to restart
	podsToDelete, instances := getPodsToRestart(podList.Items, dtc, instance)

	// Workaround: 'instances' can be null, making DeepEqual() return false when comparing against an empty map instance.
	// So, compare as long there is data.
	if (len(instances) > 0 || len(instance.Status.Instances) > 0) && !reflect.DeepEqual(instances, instance.Status.Instances) {
		logger.Info("oneagent pod instances changed", "status", instance.Status)
		updateCR = true
		instance.Status.Instances = instances
	}

	// restart daemonset
	err = r.deletePods(logger, instance, podsToDelete)
	if err != nil {
		logger.Error(err, "failed to update version")
		return updateCR, err
	}

	return updateCR, nil
}

func (r *ReconcileOneAgent) updateCR(instance *dynatracev1alpha1.OneAgent) error {
	instance.Status.UpdatedTimestamp = metav1.Now()

	newSpec := instance.Spec
	instance.Spec = dynatracev1alpha1.OneAgentSpec{}

	if err := r.client.Status().Update(context.TODO(), instance); err != nil {
		return err
	}

	instance.Spec = newSpec

	return r.client.Update(context.TODO(), instance)
}

func newDaemonSetForCR(instance *dynatracev1alpha1.OneAgent) *appsv1.DaemonSet {
	selector := buildLabels(instance.Name)
	podSpec := newPodSpecForCR(instance)

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Labels:    selector,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec:       podSpec,
			},
		},
	}
}

func newPodSpecForCR(instance *dynatracev1alpha1.OneAgent) corev1.PodSpec {
	trueVar := true

	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Args:            instance.Spec.Args,
			Env:             instance.Spec.Env,
			Image:           instance.Spec.Image,
			ImagePullPolicy: corev1.PullAlways,
			Name:            "dynatrace-oneagent",
			ReadinessProbe: &corev1.Probe{
				Handler: corev1.Handler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"/bin/sh", "-c", "grep -q oneagentwatchdo /proc/[0-9]*/stat",
						},
					},
				},
				InitialDelaySeconds: 30,
				PeriodSeconds:       30,
				TimeoutSeconds:      1,
			},
			Resources: instance.Spec.Resources,
			SecurityContext: &corev1.SecurityContext{
				Privileged: &trueVar,
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "host-root",
				MountPath: "/mnt/root",
			}},
		}},
		HostNetwork:        true,
		HostPID:            true,
		HostIPC:            true,
		NodeSelector:       instance.Spec.NodeSelector,
		PriorityClassName:  instance.Spec.PriorityClassName,
		ServiceAccountName: "dynatrace-oneagent",
		Tolerations:        instance.Spec.Tolerations,
		DNSPolicy:          instance.Spec.DNSPolicy,
		Volumes: []corev1.Volume{{
			Name: "host-root",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/",
				},
			},
		}},
	}
}

// deletePods deletes a list of pods
//
// Returns an error in the following conditions:
//  - failure on object deletion
//  - timeout on waiting for ready state
func (r *ReconcileOneAgent) deletePods(logger logr.Logger, instance *dynatracev1alpha1.OneAgent, pods []corev1.Pod) error {
	for _, pod := range pods {
		logger.Info("deleting pod", "pod", pod.Name, "node", pod.Spec.NodeName)

		// delete pod
		err := r.client.Delete(context.TODO(), &pod)
		if err != nil {
			return err
		}

		logger.Info("waiting until pod is ready on node", "node", pod.Spec.NodeName)

		// wait for pod on node to get "Running" again
		if err := r.waitPodReadyState(instance, pod); err != nil {
			return err
		}

		logger.Info("pod recreated successfully on node", "node", pod.Spec.NodeName)
	}

	return nil
}

func (r *ReconcileOneAgent) waitPodReadyState(instance *dynatracev1alpha1.OneAgent, pod corev1.Pod) error {
	var status error

	listOps := []client.ListOption{
		client.InNamespace(instance.Namespace),
		client.MatchingLabels(buildLabels(instance.Name)),
	}

	for splay := uint16(0); splay < *instance.Spec.WaitReadySeconds; splay += splayTimeSeconds {
		time.Sleep(time.Duration(splayTimeSeconds) * time.Second)

		// The actual selector we need is,
		// "spec.nodeName=<pod.Spec.NodeName>,status.phase=Running,metadata.name!=<pod.Name>"
		//
		// However, the client falls back to a cached implementation for .List() after the first attempt, which
		// is not able to handle our query so the function fails. Because of this, we're getting all the pods and
		// filtering it ourselves.
		podList := &corev1.PodList{}
		status = r.client.List(context.TODO(), podList, listOps...)
		if status != nil {
			continue
		}

		var foundPods []*corev1.Pod
		for i := range podList.Items {
			p := &podList.Items[i]
			if p.Spec.NodeName != pod.Spec.NodeName || p.Status.Phase != corev1.PodRunning ||
				p.ObjectMeta.Name == pod.Name {
				continue
			}
			foundPods = append(foundPods, p)
		}

		if n := len(foundPods); n == 0 {
			status = fmt.Errorf("waiting for pod to be recreated on node: %s", pod.Spec.NodeName)
		} else if n == 1 && getPodReadyState(foundPods[0]) {
			break
		} else if n > 1 {
			status = fmt.Errorf("too many pods found: expected=1 actual=%d", n)
		}
	}

	return status
}
