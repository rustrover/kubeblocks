/*
Copyright (C) 2022-2024 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package dataprotection

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	vsv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v3/apis/volumesnapshot/v1beta1"
	vsv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/apecloud/kubeblocks/apis/apps/v1alpha1"
	dpv1alpha1 "github.com/apecloud/kubeblocks/apis/dataprotection/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/constant"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
	"github.com/apecloud/kubeblocks/pkg/dataprotection/action"
	dpbackup "github.com/apecloud/kubeblocks/pkg/dataprotection/backup"
	dptypes "github.com/apecloud/kubeblocks/pkg/dataprotection/types"
	dputils "github.com/apecloud/kubeblocks/pkg/dataprotection/utils"
	"github.com/apecloud/kubeblocks/pkg/dataprotection/utils/boolptr"
	viper "github.com/apecloud/kubeblocks/pkg/viperx"
)

// BackupReconciler reconciles a Backup object
type BackupReconciler struct {
	client.Client
	Scheme     *k8sruntime.Scheme
	Recorder   record.EventRecorder
	RestConfig *rest.Config
	clock      clock.RealClock
}

// +kubebuilder:rbac:groups=dataprotection.kubeblocks.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dataprotection.kubeblocks.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dataprotection.kubeblocks.io,resources=backups/finalizers,verbs=update

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots/finalizers,verbs=update;patch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses/finalizers,verbs=update;patch

// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the backup closer to the desired state.
func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// setup common request context
	reqCtx := intctrlutil.RequestCtx{
		Ctx:      ctx,
		Req:      req,
		Log:      log.FromContext(ctx).WithValues("backup", req.NamespacedName),
		Recorder: r.Recorder,
	}

	// get backup object, and return if not found
	backup := &dpv1alpha1.Backup{}
	if err := r.Client.Get(reqCtx.Ctx, reqCtx.Req.NamespacedName, backup); err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
	}

	reqCtx.Log.V(1).Info("reconcile", "backup", req.NamespacedName, "phase", backup.Status.Phase)

	// if backup is being deleted, set backup phase to Deleting. The backup
	// reference workloads, data and volume snapshots will be deleted by controller
	// later when the backup status.phase is deleting.
	if !backup.GetDeletionTimestamp().IsZero() && backup.Status.Phase != dpv1alpha1.BackupPhaseDeleting {
		patch := client.MergeFrom(backup.DeepCopy())
		backup.Status.Phase = dpv1alpha1.BackupPhaseDeleting
		if err := r.Client.Status().Patch(reqCtx.Ctx, backup, patch); err != nil {
			return intctrlutil.RequeueWithError(err, reqCtx.Log, "")
		}
	}

	switch backup.Status.Phase {
	case "", dpv1alpha1.BackupPhaseNew:
		return r.handleNewPhase(reqCtx, backup)
	case dpv1alpha1.BackupPhaseRunning:
		return r.handleRunningPhase(reqCtx, backup)
	case dpv1alpha1.BackupPhaseCompleted:
		return r.handleCompletedPhase(reqCtx, backup)
	case dpv1alpha1.BackupPhaseDeleting:
		return r.handleDeletingPhase(reqCtx, backup)
	case dpv1alpha1.BackupPhaseFailed:
		if backup.Labels[dptypes.BackupTypeLabelKey] == string(dpv1alpha1.BackupTypeContinuous) {
			return r.handleRunningPhase(reqCtx, backup)
		}
		return intctrlutil.Reconciled()
	default:
		return intctrlutil.Reconciled()
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := intctrlutil.NewNamespacedControllerManagedBy(mgr).
		For(&dpv1alpha1.Backup{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: viper.GetInt(maxConcurDataProtectionReconKey),
		}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.filterBackupPods)).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.parseBackupJob))

	if dputils.SupportsVolumeSnapshotV1() {
		b.Owns(&vsv1.VolumeSnapshot{}, builder.Predicates{})
	} else {
		b.Owns(&vsv1beta1.VolumeSnapshot{}, builder.Predicates{})
	}
	return b.Complete(r)
}

func (r *BackupReconciler) filterBackupPods(ctx context.Context, obj client.Object) []reconcile.Request {
	var requests []reconcile.Request
	labels := obj.GetLabels()
	if v, ok := labels[constant.AppManagedByLabelKey]; !ok || v != constant.AppName {
		return requests
	}
	backupName, ok := labels[dptypes.BackupNameLabelKey]
	if !ok {
		return requests
	}
	for _, v := range obj.GetOwnerReferences() {
		if v.Kind == constant.StatefulSetKind && v.Name == backupName {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: obj.GetNamespace(),
					Name:      backupName,
				},
			})
			break
		}
	}
	return requests
}

func (r *BackupReconciler) parseBackupJob(_ context.Context, object client.Object) []reconcile.Request {
	job := object.(*batchv1.Job)
	var requests []reconcile.Request
	backupName := job.Labels[dptypes.BackupNameLabelKey]
	backupNamespace := job.Labels[dptypes.BackupNamespaceLabelKey]
	if backupName != "" && backupNamespace != "" {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: backupNamespace,
				Name:      backupName,
			},
		})
	}
	return requests
}

// deleteBackupFiles deletes the backup files stored in backup repository.
func (r *BackupReconciler) deleteBackupFiles(reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) error {
	deleteBackup := func() error {
		// remove backup finalizers to delete it
		patch := client.MergeFrom(backup.DeepCopy())
		controllerutil.RemoveFinalizer(backup, dptypes.DataProtectionFinalizerName)
		return r.Patch(reqCtx.Ctx, backup, patch)
	}

	deleter := &dpbackup.Deleter{
		RequestCtx: reqCtx,
		Client:     r.Client,
		Scheme:     r.Scheme,
	}

	saName, err := EnsureWorkerServiceAccount(reqCtx, r.Client, backup.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get worker service account: %w", err)
	}
	deleter.WorkerServiceAccount = saName

	status, err := deleter.DeleteBackupFiles(backup)
	switch status {
	case dpbackup.DeletionStatusSucceeded:
		return deleteBackup()
	case dpbackup.DeletionStatusFailed:
		failureReason := err.Error()
		if backup.Status.FailureReason == failureReason {
			return nil
		}
		backupPatch := client.MergeFrom(backup.DeepCopy())
		backup.Status.FailureReason = failureReason
		r.Recorder.Event(backup, corev1.EventTypeWarning, "DeleteBackupFilesFailed", failureReason)
		return r.Status().Patch(reqCtx.Ctx, backup, backupPatch)
	case dpbackup.DeletionStatusDeleting,
		dpbackup.DeletionStatusUnknown:
		// wait for the deletion job completed
		return err
	}
	return err
}

// handleDeletingPhase handles the deletion of backup. It will delete the backup CR
// and the backup workload(job).
func (r *BackupReconciler) handleDeletingPhase(reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) (ctrl.Result, error) {
	// if backup phase is Deleting, delete the backup reference workloads,
	// backup data stored in backup repository and volume snapshots.
	// TODO(ldm): if backup is being used by restore, do not delete it.
	if err := r.deleteExternalResources(reqCtx, backup); err != nil {
		return intctrlutil.RequeueWithError(err, reqCtx.Log, "")
	}

	if backup.Spec.DeletionPolicy == dpv1alpha1.BackupDeletionPolicyRetain {
		r.Recorder.Event(backup, corev1.EventTypeWarning, "Retain", "can not delete the backup if deletionPolicy is Retain")
		return intctrlutil.Reconciled()
	}

	if err := r.deleteVolumeSnapshots(reqCtx, backup); err != nil {
		return intctrlutil.RequeueWithError(err, reqCtx.Log, "")
	}

	if err := r.deleteBackupFiles(reqCtx, backup); err != nil {
		return intctrlutil.RequeueWithError(err, reqCtx.Log, "")
	}
	return intctrlutil.Reconciled()
}

func (r *BackupReconciler) handleNewPhase(
	reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) (ctrl.Result, error) {
	request, err := r.prepareBackupRequest(reqCtx, backup)
	if err != nil {
		return r.updateStatusIfFailed(reqCtx, backup.DeepCopy(), backup, err)
	}

	// set and patch backup object meta, including labels, annotations and finalizers
	// if the backup object meta is changed, the backup object will be patched.
	if wait, err := PatchBackupObjectMeta(backup, request); err != nil {
		return r.updateStatusIfFailed(reqCtx, backup, request.Backup, err)
	} else if wait {
		return intctrlutil.Reconciled()
	}

	// set and patch backup status
	if err = r.patchBackupStatus(backup, request); err != nil {
		return r.updateStatusIfFailed(reqCtx, backup, request.Backup, err)
	}
	return intctrlutil.Reconciled()
}

// prepareBackupRequest prepares a request for a backup, with all references to
// other kubernetes objects, and validate them.
func (r *BackupReconciler) prepareBackupRequest(
	reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) (*dpbackup.Request, error) {
	request := &dpbackup.Request{
		Backup:     backup.DeepCopy(),
		RequestCtx: reqCtx,
		Client:     r.Client,
	}

	if request.Annotations == nil {
		request.Annotations = make(map[string]string)
	}

	if request.Labels == nil {
		request.Labels = make(map[string]string)
	}

	backupPolicy, err := dputils.GetBackupPolicyByName(reqCtx, r.Client, backup.Spec.BackupPolicyName)
	if err != nil {
		return nil, err
	}

	backupMethod := dputils.GetBackupMethodByName(backup.Spec.BackupMethod, backupPolicy)
	if backupMethod == nil {
		return nil, intctrlutil.NewNotFound("backupMethod: %s not found",
			backup.Spec.BackupMethod)
	}

	// backupMethod should specify snapshotVolumes or actionSetName, if we take
	// snapshots to back up volumes, the snapshotVolumes should be set to true
	// and the actionSetName is not required, if we do not take snapshots to back
	// up volumes, the actionSetName is required.
	snapshotVolumes := boolptr.IsSetToTrue(backupMethod.SnapshotVolumes)
	if !snapshotVolumes && backupMethod.ActionSetName == "" {
		return nil, fmt.Errorf("backup method %s should specify snapshotVolumes or actionSetName", backupMethod.Name)
	}

	if backupMethod.ActionSetName != "" {
		actionSet, err := dputils.GetActionSetByName(reqCtx, r.Client, backupMethod.ActionSetName)
		if err != nil {
			return nil, err
		}
		request.ActionSet = actionSet
	}

	// check encryption config
	if backupPolicy.Spec.EncryptionConfig != nil {
		secretKeyRef := backupPolicy.Spec.EncryptionConfig.PassPhraseSecretKeyRef
		if secretKeyRef == nil {
			return nil, fmt.Errorf("encryptionConfig.passPhraseSecretKeyRef if empty")
		}
		err := checkSecretKeyRef(reqCtx, r.Client, request.Namespace, secretKeyRef)
		if err != nil {
			return nil, fmt.Errorf("failed to check encryption key reference: %w", err)
		}
	}

	request.BackupPolicy = backupPolicy
	if !snapshotVolumes {
		// if use volume snapshot, ignore backup repo
		if err = HandleBackupRepo(request); err != nil {
			return nil, err
		}
	}
	request.BackupMethod = backupMethod

	targetPods, err := GetTargetPods(reqCtx, r.Client,
		backup.Annotations[dptypes.BackupTargetPodLabelKey], backupMethod, backupPolicy)
	if err != nil || len(targetPods) == 0 {
		return nil, fmt.Errorf("failed to get target pods by backup policy %s/%s",
			backupPolicy.Namespace, backupPolicy.Name)
	}
	request.TargetPods = targetPods

	saName := backupPolicy.Spec.Target.ServiceAccountName
	if saName == "" {
		saName, err = EnsureWorkerServiceAccount(reqCtx, r.Client, backup.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to get worker service account: %w", err)
		}
	}
	request.WorkerServiceAccount = saName

	return request, nil
}

func (r *BackupReconciler) patchBackupStatus(
	original *dpv1alpha1.Backup,
	request *dpbackup.Request) error {
	request.Status.FormatVersion = dpbackup.FormatVersion
	request.Status.Path = dpbackup.BuildBackupPath(request.Backup, request.BackupPolicy.Spec.PathPrefix)
	request.Status.Target = request.BackupPolicy.Spec.Target
	request.Status.BackupMethod = request.BackupMethod
	if request.BackupRepo != nil {
		request.Status.BackupRepoName = request.BackupRepo.Name
	}
	if request.BackupRepoPVC != nil {
		request.Status.PersistentVolumeClaimName = request.BackupRepoPVC.Name
	}
	if request.BackupPolicy.Spec.UseKopia {
		request.Status.KopiaRepoPath = dpbackup.BuildKopiaRepoPath(request.Backup, request.BackupPolicy.Spec.PathPrefix)
	}
	if request.BackupPolicy.Spec.EncryptionConfig != nil {
		request.Status.EncryptionConfig = request.BackupPolicy.Spec.EncryptionConfig
	}
	// init action status
	actions, err := request.BuildActions()
	if err != nil {
		return err
	}
	request.Status.Actions = make([]dpv1alpha1.ActionStatus, len(actions))
	for i, act := range actions {
		request.Status.Actions[i] = dpv1alpha1.ActionStatus{
			Name:       act.GetName(),
			Phase:      dpv1alpha1.ActionPhaseNew,
			ActionType: act.Type(),
		}
	}

	// update phase to running
	request.Status.Phase = dpv1alpha1.BackupPhaseRunning
	request.Status.StartTimestamp = &metav1.Time{Time: r.clock.Now().UTC()}

	if err = dpbackup.SetExpirationByCreationTime(request.Backup); err != nil {
		return err
	}
	return r.Client.Status().Patch(request.Ctx, request.Backup, client.MergeFrom(original))
}

func (r *BackupReconciler) handleRunningPhase(
	reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) (ctrl.Result, error) {
	request, err := r.prepareBackupRequest(reqCtx, backup)
	if err != nil {
		return r.updateStatusIfFailed(reqCtx, backup.DeepCopy(), backup, err)
	}

	if request.ActionSet != nil && request.ActionSet.Spec.BackupType == dpv1alpha1.BackupTypeContinuous {
		// check if the continuous backup is completed.
		if completed, err := r.checkIsCompletedDuringRunning(reqCtx, request); err != nil {
			return RecorderEventAndRequeue(reqCtx, r.Recorder, backup, err)
		} else if completed {
			return intctrlutil.Reconciled()
		}
	}

	// there are actions not completed, continue to handle following actions
	actions, err := request.BuildActions()
	if err != nil {
		return r.updateStatusIfFailed(reqCtx, backup, request.Backup, err)
	}

	actionCtx := action.ActionContext{
		Ctx:              reqCtx.Ctx,
		Client:           r.Client,
		Recorder:         r.Recorder,
		Scheme:           r.Scheme,
		RestClientConfig: r.RestConfig,
	}

	// check all actions status, if any action failed, update backup status to failed
	// if all actions completed, update backup status to completed, otherwise,
	// continue to handle following actions.
	for i, act := range actions {
		status, err := act.Execute(actionCtx)
		if err != nil {
			return r.updateStatusIfFailed(reqCtx, backup, request.Backup, err)
		}
		request.Status.Actions[i] = mergeActionStatus(&request.Status.Actions[i], status)

		switch status.Phase {
		case dpv1alpha1.ActionPhaseCompleted:
			updateBackupStatusByActionStatus(&request.Status)
			continue
		case dpv1alpha1.ActionPhaseFailed:
			return r.updateStatusIfFailed(reqCtx, backup, request.Backup,
				fmt.Errorf("action %s failed, %s", act.GetName(), status.FailureReason))
		case dpv1alpha1.ActionPhaseRunning:
			// update status
			if err = r.Client.Status().Patch(reqCtx.Ctx, request.Backup, client.MergeFrom(backup)); err != nil {
				return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
			}
			return intctrlutil.Reconciled()
		}
	}

	// all actions completed, update backup status to completed
	request.Status.Phase = dpv1alpha1.BackupPhaseCompleted
	request.Status.CompletionTimestamp = &metav1.Time{Time: r.clock.Now().UTC()}
	if !request.Status.StartTimestamp.IsZero() {
		// round the duration to a multiple of seconds.
		duration := request.Status.CompletionTimestamp.Sub(request.Status.StartTimestamp.Time).Round(time.Second)
		request.Status.Duration = &metav1.Duration{Duration: duration}
	}
	if request.Spec.RetentionPeriod != "" {
		// set expiration time
		duration, err := request.Spec.RetentionPeriod.ToDuration()
		if err != nil {
			return r.updateStatusIfFailed(reqCtx, backup, request.Backup, fmt.Errorf("failed to parse retention period %s, %v", request.Spec.RetentionPeriod, err))
		}
		if duration.Seconds() > 0 {
			request.Status.Expiration = &metav1.Time{
				Time: request.Status.CompletionTimestamp.Add(duration),
			}
		}
	}
	r.Recorder.Event(backup, corev1.EventTypeNormal, "CreatedBackup", "Completed backup")
	if err = r.Client.Status().Patch(reqCtx.Ctx, request.Backup, client.MergeFrom(backup)); err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
	}
	return intctrlutil.Reconciled()
}

// checkIsCompletedDuringRunning when continuous schedule is disabled or cluster has been deleted,
// backup phase should be Completed.
func (r *BackupReconciler) checkIsCompletedDuringRunning(reqCtx intctrlutil.RequestCtx,
	request *dpbackup.Request) (bool, error) {
	backupScheduleList := &dpv1alpha1.BackupScheduleList{}
	if err := r.Client.List(reqCtx.Ctx, backupScheduleList, client.MatchingLabels{
		dptypes.BackupPolicyLabelKey: request.Backup.Spec.BackupPolicyName,
	}); err != nil {
		return false, err
	}
	var (
		enabled             *bool
		targetClusterExists = true
	)
	// check if Continuous backupMethod is enabled
	for _, v := range backupScheduleList.Items {
		for _, method := range v.Spec.Schedules {
			if method.BackupMethod == request.Spec.BackupMethod {
				enabled = method.Enabled
				break
			}
		}
	}
	// check if target cluster exits
	clusterName := request.Labels[constant.AppInstanceLabelKey]
	if clusterName != "" {
		cluster := &appsv1alpha1.Cluster{}
		var err error
		targetClusterExists, err = intctrlutil.CheckResourceExists(reqCtx.Ctx, r.Client,
			client.ObjectKey{Name: clusterName, Namespace: request.Namespace}, cluster)
		if err != nil {
			return false, err
		}
	}
	if boolptr.IsSetToTrue(enabled) && targetClusterExists {
		return false, nil
	}
	patch := client.MergeFrom(request.Backup.DeepCopy())
	request.Status.Phase = dpv1alpha1.BackupPhaseCompleted
	request.Status.CompletionTimestamp = &metav1.Time{Time: r.clock.Now().UTC()}
	_ = dpbackup.SetExpirationByCreationTime(request.Backup)
	if !request.Status.StartTimestamp.IsZero() {
		// round the duration to a multiple of seconds.
		duration := request.Status.CompletionTimestamp.Sub(request.Status.StartTimestamp.Time).Round(time.Second)
		request.Status.Duration = &metav1.Duration{Duration: duration}
	}
	return true, r.Client.Status().Patch(reqCtx.Ctx, request.Backup, patch)
}

// handleCompletedPhase handles the backup object in completed phase.
// It will delete the reference workloads.
func (r *BackupReconciler) handleCompletedPhase(
	reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) (ctrl.Result, error) {
	if err := r.deleteExternalResources(reqCtx, backup); err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
	}

	return intctrlutil.Reconciled()
}

func (r *BackupReconciler) updateStatusIfFailed(
	reqCtx intctrlutil.RequestCtx,
	original *dpv1alpha1.Backup,
	backup *dpv1alpha1.Backup,
	err error) (ctrl.Result, error) {
	if intctrlutil.IsTargetError(err, intctrlutil.ErrorTypeRequeue) {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
	}
	sendWarningEventForError(r.Recorder, backup, err)
	backup.Status.Phase = dpv1alpha1.BackupPhaseFailed
	backup.Status.FailureReason = err.Error()

	// set expiration time for failed backup, make sure the failed backup will be
	// deleted after the expiration time.
	_ = dpbackup.SetExpirationByCreationTime(backup)

	if errUpdate := r.Client.Status().Patch(reqCtx.Ctx, backup, client.MergeFrom(original)); errUpdate != nil {
		return intctrlutil.CheckedRequeueWithError(errUpdate, reqCtx.Log, "")
	}
	return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
}

// deleteExternalJobs deletes the external jobs.
func (r *BackupReconciler) deleteExternalJobs(reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) error {
	labels := dpbackup.BuildBackupWorkloadLabels(backup)
	if err := deleteRelatedJobs(reqCtx, r.Client, backup.Namespace, labels); err != nil {
		return err
	}
	return deleteRelatedJobs(reqCtx, r.Client, viper.GetString(constant.CfgKeyCtrlrMgrNS), labels)
}

func (r *BackupReconciler) deleteVolumeSnapshots(reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) error {
	deleter := &dpbackup.Deleter{
		RequestCtx: reqCtx,
		Client:     r.Client,
	}
	return deleter.DeleteVolumeSnapshots(backup)
}

// deleteExternalStatefulSet deletes the external statefulSet.
func (r *BackupReconciler) deleteExternalStatefulSet(reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) error {
	key := client.ObjectKey{
		Namespace: backup.Namespace,
		Name:      backup.Name,
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Client.Get(reqCtx.Ctx, key, sts); err != nil {
		return client.IgnoreNotFound(err)
	}

	patch := client.MergeFrom(sts.DeepCopy())
	controllerutil.RemoveFinalizer(sts, dptypes.DataProtectionFinalizerName)
	if err := r.Client.Patch(reqCtx.Ctx, sts, patch); err != nil {
		return err
	}
	reqCtx.Log.V(1).Info("delete statefulSet", "statefulSet", sts)
	return intctrlutil.BackgroundDeleteObject(r.Client, reqCtx.Ctx, sts)
}

// deleteExternalResources deletes the external workloads that execute backup.
// Currently, it only supports two types of workloads: job.
func (r *BackupReconciler) deleteExternalResources(
	reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) error {
	if err := r.deleteExternalJobs(reqCtx, backup); err != nil {
		return err
	}
	return r.deleteExternalStatefulSet(reqCtx, backup)
}

// PatchBackupObjectMeta patches backup object metaObject include cluster snapshot.
func PatchBackupObjectMeta(
	original *dpv1alpha1.Backup,
	request *dpbackup.Request) (bool, error) {
	targetPod := request.TargetPods[0]

	// get KubeBlocks cluster and set labels and annotations for backup
	// TODO(ldm): we should remove this dependency of cluster in the future
	cluster := getCluster(request.Ctx, request.Client, targetPod)
	if cluster != nil {
		if err := setClusterSnapshotAnnotation(request.Backup, cluster); err != nil {
			return false, err
		}
		if err := setConnectionPasswordAnnotation(request); err != nil {
			return false, err
		}
		request.Labels[dptypes.ClusterUIDLabelKey] = string(cluster.UID)
	}

	for _, v := range getClusterLabelKeys() {
		request.Labels[v] = targetPod.Labels[v]
	}

	request.Labels[constant.AppManagedByLabelKey] = dptypes.AppName
	request.Labels[dptypes.BackupTypeLabelKey] = request.GetBackupType()
	request.Labels[dptypes.BackupPolicyLabelKey] = request.Spec.BackupPolicyName
	// wait for the backup repo controller to prepare the essential resource.
	wait := false
	if request.BackupRepo != nil {
		request.Labels[dataProtectionBackupRepoKey] = request.BackupRepo.Name
		if (request.BackupRepo.AccessByMount() && request.BackupRepoPVC == nil) ||
			(request.BackupRepo.AccessByTool() && request.ToolConfigSecret == nil) {
			request.Labels[dataProtectionWaitRepoPreparationKey] = trueVal
			wait = true
		}
	}

	// set annotations
	request.Annotations[dptypes.BackupTargetPodLabelKey] = targetPod.Name

	// set finalizer
	controllerutil.AddFinalizer(request.Backup, dptypes.DataProtectionFinalizerName)

	if reflect.DeepEqual(original.ObjectMeta, request.ObjectMeta) {
		return wait, nil
	}

	return wait, request.Client.Patch(request.Ctx, request.Backup, client.MergeFrom(original))
}

func mergeActionStatus(original, new *dpv1alpha1.ActionStatus) dpv1alpha1.ActionStatus {
	as := new.DeepCopy()
	if original.StartTimestamp != nil {
		as.StartTimestamp = original.StartTimestamp
	}
	return *as
}

func updateBackupStatusByActionStatus(backupStatus *dpv1alpha1.BackupStatus) {
	for _, act := range backupStatus.Actions {
		if act.TotalSize != "" && backupStatus.TotalSize == "" {
			backupStatus.TotalSize = act.TotalSize
		}
		if act.TimeRange != nil && backupStatus.TimeRange == nil {
			backupStatus.TimeRange = act.TimeRange
		}
	}
}

// setConnectionPasswordAnnotation sets the encrypted password of the connection credential to the backup's annotations
func setConnectionPasswordAnnotation(request *dpbackup.Request) error {
	encryptPassword := func() (string, error) {
		target := request.BackupPolicy.Spec.Target
		if target == nil || target.ConnectionCredential == nil {
			return "", nil
		}
		secret := &corev1.Secret{}
		if err := request.Client.Get(request.Ctx, client.ObjectKey{Name: target.ConnectionCredential.SecretName, Namespace: request.Namespace}, secret); err != nil {
			return "", err
		}
		e := intctrlutil.NewEncryptor(viper.GetString(constant.CfgKeyDPEncryptionKey))
		ciphertext, err := e.Encrypt(secret.Data[target.ConnectionCredential.PasswordKey])
		if err != nil {
			return "", err
		}
		return ciphertext, nil
	}
	// save the connection credential password for cluster.
	ciphertext, err := encryptPassword()
	if err != nil {
		return err
	}
	if ciphertext != "" {
		request.Backup.Annotations[dptypes.ConnectionPasswordAnnotationKey] = ciphertext
	}
	return nil
}

// getClusterObjectString gets the cluster object and convert it to string.
func getClusterObjectString(cluster *appsv1alpha1.Cluster) (*string, error) {
	// maintain only the cluster's spec and name/namespace.
	newCluster := &appsv1alpha1.Cluster{
		Spec: cluster.Spec,
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      cluster.Name,
		},
		TypeMeta: cluster.TypeMeta,
	}
	if v, ok := cluster.Annotations[constant.ExtraEnvAnnotationKey]; ok {
		newCluster.Annotations = map[string]string{
			constant.ExtraEnvAnnotationKey: v,
		}
	}
	clusterBytes, err := json.Marshal(newCluster)
	if err != nil {
		return nil, err
	}
	clusterString := string(clusterBytes)
	return &clusterString, nil
}

// setClusterSnapshotAnnotation sets the snapshot of cluster to the backup's annotations.
func setClusterSnapshotAnnotation(backup *dpv1alpha1.Backup, cluster *appsv1alpha1.Cluster) error {
	clusterString, err := getClusterObjectString(cluster)
	if err != nil {
		return err
	}
	if clusterString == nil {
		return nil
	}
	if backup.Annotations == nil {
		backup.Annotations = map[string]string{}
	}
	backup.Annotations[constant.ClusterSnapshotAnnotationKey] = *clusterString
	return nil
}
