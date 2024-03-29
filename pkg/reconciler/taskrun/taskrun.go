package taskrun

import (
	"context"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"knative.dev/pkg/apis"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

const (
	//TODO eventually we'll need to decide if we want to make this tuneable
	contextTimeout = 300 * time.Second

	ConfigMapLabel = "build.appstudio.redhat.com/multi-arch-config"

	//user level labels that specify a task needs to be executed on a remote host
	TargetArchitectureLabel = "build.appstudio.redhat.com/target-architecture"
	MultiArchLabel          = "build.appstudio.redhat.com/multi-arch-required"

	AssignedHost           = "build.appstudio.redhat.com/assigned-host"
	ProvisionTaskName      = "build.appstudio.redhat.com/provision-task-name"
	ProvisionTaskNamespace = "build.appstudio.redhat.com/provision-task-namespace"
	WaitingForArchLabel    = "build.appstudio.redhat.com/waiting-for-arch"
	PipelineFinalizer      = "appstudio.io/multi-arch-finalizer"
	HostConfig             = "host-config"

	TaskTypeLabel     = "build.appstudio.redhat.com/task-type"
	TaskTypeProvision = "provision"
	TaskTypeClean     = "clean"

	ServiceAccountName = "multi-arch-controller"
)

type ReconcileTaskRun struct {
	client            client.Client
	scheme            *runtime.Scheme
	eventRecorder     record.EventRecorder
	operatorNamespace string
}

func newReconciler(mgr ctrl.Manager, operatorNamespace string) reconcile.Reconciler {
	return &ReconcileTaskRun{
		client:            mgr.GetClient(),
		scheme:            mgr.GetScheme(),
		eventRecorder:     mgr.GetEventRecorderFor("ComponentBuild"),
		operatorNamespace: operatorNamespace,
	}
}

func (r *ReconcileTaskRun) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	// Set the ctx to be Background, as the top-level context for incoming requests.
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	log := ctrl.Log.WithName("taskrun").WithValues("request", request.NamespacedName)

	pr := v1beta1.TaskRun{}
	prerr := r.client.Get(ctx, request.NamespacedName, &pr)
	if prerr != nil {
		if !errors.IsNotFound(prerr) {
			log.Error(prerr, "Reconcile key %s as TaskRun unexpected error", request.NamespacedName.String())
			return ctrl.Result{}, prerr
		}
	}
	if prerr != nil {
		msg := "Reconcile key received not found errors for TaskRuns (probably deleted): " + request.NamespacedName.String()
		log.Info(msg)
		return ctrl.Result{}, nil
	}

	switch {
	case prerr == nil:
		return r.handleTaskRunReceived(ctx, &log, &pr)
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileTaskRun) handleTaskRunReceived(ctx context.Context, log *logr.Logger, tr *v1beta1.TaskRun) (reconcile.Result, error) {
	if tr.Labels == nil {
		return reconcile.Result{}, nil
	}
	taskType := tr.Labels[TaskTypeLabel]
	if taskType == TaskTypeClean {
		return r.handleCleanTask(ctx, log, tr)
	}
	if taskType == TaskTypeProvision {
		return r.handleProvisionTask(ctx, log, tr)
	}

	if tr.Labels == nil || tr.Labels[TargetArchitectureLabel] == "" || tr.Labels[MultiArchLabel] == "" {
		//this is not something we need to be concerned with
		return reconcile.Result{}, nil
	}
	return r.handleUserTask(ctx, log, tr)
}

// called when a task has finished, we look for waiting tasks
// and then potentially requeue one of them
func (r *ReconcileTaskRun) handleWaitingTasks(ctx context.Context, log *logr.Logger, arch string) (reconcile.Result, error) {

	//try and requeue a waiting task if one exists
	taskList := v1beta1.TaskRunList{}

	err := r.client.List(ctx, &taskList, client.MatchingLabels{WaitingForArchLabel: arch})
	if err != nil {
		return reconcile.Result{}, err
	}
	var oldest *v1beta1.TaskRun
	var oldestTs time.Time
	for i := range taskList.Items {
		tr := taskList.Items[i]
		if oldest == nil || oldestTs.After(tr.CreationTimestamp.Time) {
			oldestTs = tr.CreationTimestamp.Time
			oldest = &tr
		}
	}
	if oldest != nil {
		//remove the waiting label, which will trigger a requeue
		delete(oldest.Labels, WaitingForArchLabel)
		return reconcile.Result{}, r.client.Update(ctx, oldest)
	}
	return reconcile.Result{}, nil

}

func (r *ReconcileTaskRun) handleCleanTask(ctx context.Context, log *logr.Logger, tr *v1beta1.TaskRun) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (r *ReconcileTaskRun) handleProvisionTask(ctx context.Context, log *logr.Logger, tr *v1beta1.TaskRun) (reconcile.Result, error) {

	if tr.Status.CompletionTime == nil {
		return reconcile.Result{}, nil
	}
	success := tr.Status.GetCondition(apis.ConditionSucceeded).IsTrue()
	if !success {
		log.Info("provision task failed")
		//TODO: retries with different hosts
		//create a failure secret
		secretName := ""
		for _, i := range tr.Spec.Params {
			if i.Name == "SECRET_NAME" {
				secretName = i.Value.StringVal
				break
			}
		}

		secret := v12.Secret{}
		secret.Labels = map[string]string{TargetArchitectureLabel: tr.Labels[TargetArchitectureLabel]}
		secret.Namespace = tr.Namespace
		secret.Name = secretName

		secret.Data = map[string][]byte{
			"error": []byte("provisioning failed"),
		}
		err := r.client.Create(ctx, &secret)
		if err != nil {
			return reconcile.Result{}, err
		}
	} else {
		log.Info("provision task succeeded")
	}

	return reconcile.Result{}, r.client.Delete(ctx, tr)

}

func (r *ReconcileTaskRun) handleUserTask(ctx context.Context, log *logr.Logger, tr *v1beta1.TaskRun) (reconcile.Result, error) {

	secretName := "multi-arch-ssl-" + tr.Name
	if tr.Labels[AssignedHost] != "" {
		return r.handleHostAssigned(ctx, log, tr, secretName)
	} else {
		//if the PR is done we ignore it
		if tr.Status.CompletionTime != nil || tr.GetDeletionTimestamp() != nil {
			log.Info("task run already finished, not creating secret")
			return reconcile.Result{}, nil
		}

		return r.handleHostAllocation(ctx, log, tr, secretName)
	}
}

func (r *ReconcileTaskRun) handleHostAllocation(ctx context.Context, log *logr.Logger, tr *v1beta1.TaskRun, secretName string) (reconcile.Result, error) {
	log.Info("attempting to allocate host")
	targetArch := tr.Labels[TargetArchitectureLabel]

	//lets allocate a host, get the map with host info
	hosts, err := r.hostConfig(ctx, log)
	if err != nil {
		return reconcile.Result{}, err
	}
	//get all existing runs that are assigned to a host
	taskList := v1beta1.TaskRunList{}

	err = r.client.List(ctx, &taskList, client.HasLabels{AssignedHost})
	if err != nil {
		return reconcile.Result{}, err
	}
	hostCount := map[string]int{}
	for _, tr := range taskList.Items {
		host := tr.Labels[AssignedHost]
		hostCount[host] = hostCount[host] + 1
	}
	for k, v := range hostCount {
		log.Info("host count", "host", k, "count", v)
	}

	//now select the host with the most free spots
	//this algorithm is not very complex

	var selected *Host
	freeSpots := 0
	for k, v := range hosts {
		if v.Arch != targetArch {
			log.Info("ignoring host", "host", k, "targetArch", targetArch, "hostArch", v.Arch)
			continue
		}
		free := v.Concurrency - hostCount[k]

		log.Info("considering host", "host", k, "freeSlots", free)
		if free > freeSpots {
			selected = v
			freeSpots = free
		}
	}
	if selected == nil {
		log.Info("no host found, waiting for one to become available")
		//no host available
		//add the waiting label
		//TODO: is the requeue actually a good idea?
		tr.Labels[WaitingForArchLabel] = targetArch
		return reconcile.Result{RequeueAfter: time.Minute}, r.client.Update(ctx, tr)
	}
	tr.Labels[AssignedHost] = selected.Name

	//kick off the provisioning task
	provision := v1beta1.TaskRun{}
	provision.GenerateName = "provision-task"
	provision.Namespace = r.operatorNamespace
	provision.Labels = map[string]string{TaskTypeLabel: TaskTypeProvision, TargetArchitectureLabel: targetArch}
	provision.Spec.TaskRef = &v1beta1.TaskRef{Name: "provision-shared-host"}
	provision.Spec.Workspaces = []v1beta1.WorkspaceBinding{{Name: "ssh", Secret: &v12.SecretVolumeSource{SecretName: selected.Secret}}}
	provision.Spec.ServiceAccountName = ServiceAccountName //TODO: special service account for this
	provision.Spec.Params = []v1beta1.Param{
		{
			Name:  "SECRET_NAME",
			Value: *v1beta1.NewStructuredValues(secretName),
		},
		{
			Name:  "TASKRUN_NAME",
			Value: *v1beta1.NewStructuredValues(tr.Name),
		},
		{
			Name:  "NAMESPACE",
			Value: *v1beta1.NewStructuredValues(tr.Namespace),
		},
		{
			Name:  "HOST",
			Value: *v1beta1.NewStructuredValues(selected.Address),
		},
		{
			Name:  "USER",
			Value: *v1beta1.NewStructuredValues(selected.User),
		},
	}
	err = r.client.Create(ctx, &provision)
	if err != nil {
		return reconcile.Result{}, err
	}

	log.Info("allocated host", "host", selected.Name)
	delete(tr.Labels, WaitingForArchLabel)
	//add a finalizer to clean up the secret
	controllerutil.AddFinalizer(tr, PipelineFinalizer)
	return reconcile.Result{}, r.client.Update(ctx, tr)
}

func (r *ReconcileTaskRun) handleHostAssigned(ctx context.Context, log *logr.Logger, tr *v1beta1.TaskRun, secretName string) (reconcile.Result, error) {
	//already exists
	if tr.Status.CompletionTime != nil || tr.GetDeletionTimestamp() != nil {
		log.Info("unassigning host from task")

		selectedHost := tr.Labels[AssignedHost]
		config, err := r.hostConfig(ctx, log)
		if err != nil {
			return reconcile.Result{}, err
		}
		selected := config[selectedHost]
		if selected != nil {
			log.Info("starting cleanup task")
			//kick off the clean task
			//kick off the provisioning task
			provision := v1beta1.TaskRun{}
			provision.GenerateName = "cleanup-task"
			provision.Namespace = r.operatorNamespace
			provision.Labels = map[string]string{TaskTypeLabel: TaskTypeClean, TargetArchitectureLabel: tr.Labels[TargetArchitectureLabel]}
			provision.Spec.TaskRef = &v1beta1.TaskRef{Name: "clean-shared-host"}
			provision.Spec.Workspaces = []v1beta1.WorkspaceBinding{{Name: "ssh", Secret: &v12.SecretVolumeSource{SecretName: selected.Secret}}}
			provision.Spec.ServiceAccountName = ServiceAccountName //TODO: special service account for this
			provision.Spec.Params = []v1beta1.Param{
				{
					Name:  "SECRET_NAME",
					Value: *v1beta1.NewStructuredValues(secretName),
				},
				{
					Name:  "TASKRUN_NAME",
					Value: *v1beta1.NewStructuredValues(tr.Name),
				},
				{
					Name:  "NAMESPACE",
					Value: *v1beta1.NewStructuredValues(tr.Namespace),
				},
				{
					Name:  "HOST",
					Value: *v1beta1.NewStructuredValues(selected.Address),
				},
				{
					Name:  "USER",
					Value: *v1beta1.NewStructuredValues(selected.User),
				},
			}
			err = r.client.Create(ctx, &provision)
			if err != nil {
				return reconcile.Result{}, err
			}

		}

		secret := v12.Secret{}
		//delete the secret
		err = r.client.Get(ctx, types.NamespacedName{Namespace: tr.Namespace, Name: secretName}, &secret)
		if err == nil {
			log.Info("deleting secret from task")
			//PR is done, clean up the secret
			err := r.client.Delete(ctx, &secret)
			if err != nil {
				log.Error(err, "unable to delete secret")
			}
		} else if !errors.IsNotFound(err) {
			log.Error(err, "error deleting secret", "secret", secretName)
			return reconcile.Result{}, err
		} else {
			log.Info("could not find secret", "secret", secretName)
		}

		controllerutil.RemoveFinalizer(tr, PipelineFinalizer)
		delete(tr.Labels, AssignedHost)
		err = r.client.Update(ctx, tr)
		if err != nil {
			return reconcile.Result{}, err
		}
		return r.handleWaitingTasks(ctx, log, tr.Labels[TargetArchitectureLabel])
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileTaskRun) hostConfig(ctx context.Context, log *logr.Logger) (map[string]*Host, error) {
	cm := v12.ConfigMap{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.operatorNamespace, Name: HostConfig}, &cm)
	if err != nil {
		return nil, err
	}
	ret := map[string]*Host{}
	for k, v := range cm.Data {
		pos := strings.LastIndex(k, ".")
		if pos == -1 {
			continue
		}
		name := k[0:pos]
		key := k[pos+1:]
		host := ret[name]
		if host == nil {
			host = &Host{}
			ret[name] = host
			host.Name = name
		}
		switch key {
		case "address":
			host.Address = v
		case "user":
			host.User = v
		case "arch":
			host.Arch = v
		case "secret":
			host.Secret = v
		case "concurrency":
			atoi, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			host.Concurrency = atoi
		default:
			log.Info("unknown key", "key", key)
		}

	}
	return ret, nil
}

type Host struct {
	Address     string
	Name        string
	User        string
	Concurrency int
	Arch        string
	Secret      string
}
