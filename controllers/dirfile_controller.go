/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	filev1 "felinae98.cn/file-controller/api/v1"
)

const (
	podNamespace  = "file-keeper"
	podNameLabel  = "file-loader-pod"
	podMountPoint = "/mnt"
)

// DirFileReconciler reconciles a DirFile object
type DirFileReconciler struct {
	client.Client
	RESTClient rest.Interface
	RESTConfig *rest.Config
	Scheme     *runtime.Scheme
}

func (r *DirFileReconciler) podExec(podName string, namespace string, cmd []string) (string, string, error) {
	execReq := r.RESTClient.Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "file-loader-pod",
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
		}, runtime.NewParameterCodec(r.Scheme))

	exec, err := remotecommand.NewSPDYExecutor(r.RESTConfig, "POST", execReq.URL())
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	return stdout.String(), stderr.String(), err

}

//+kubebuilder:rbac:groups=file.felinae98.cn,resources=dirfiles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=file.felinae98.cn,resources=dirfiles/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=file.felinae98.cn,resources=dirfiles/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pod,verbs=get;list;patch;delete
//+kubebuilder:rbac:groups=core,resources=pod/exec,verbs=create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DirFile object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.1/pkg/reconcile
func (r *DirFileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconcile triggered, request: req", "req", req)
	var dirFile filev1.DirFile
	if err := r.Get(ctx, req.NamespacedName, &dirFile); err != nil {
		// DirFile object not exist
		logger.Error(err, "DirFile not found, name: Name", "Name", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// TODO(user): your logic here
	var podList corev1.PodList
	// namespace "file-keeper" is hard-encoded
	if err := r.List(ctx, &podList, client.InNamespace(podNamespace), client.MatchingLabels{"name": podNameLabel}); err != nil {
		logger.Error(err, "Pod in namespace PodNamespace with name=PodNameLabel not found", "PodNamespace", podNamespace, "PodNameLabel", podNameLabel)
		return ctrl.Result{}, err
	}
	targetStatus := make(map[string][]string)
	for _, pod := range podList.Items {
		nodeName := pod.Spec.NodeName
		podName := pod.Name
		var _ = nodeName
		dirPath := filepath.Join(podMountPoint, dirFile.Spec.Dir)
		stdout, stderr, err := r.podExec(podName, podNamespace, []string{"ls", "-a", dirPath})
		var fileList []string
		if err != nil && strings.Contains(stderr, "No such file or directory") {
			// Dir not exist
			logger.Info("Dir not exsits, creating", "dirPath", dirPath, "node", nodeName)
			_, dirCreationStderr, dirCreationErr := r.podExec(podName, podNamespace, []string{"mkdir", "-p", dirPath})
			if dirCreationErr != nil {
				logger.Error(dirCreationErr, "failed to create directory", "dirPath", dirPath, "stderr", dirCreationStderr, "node", nodeName)
				return ctrl.Result{}, dirCreationErr
			}
			fileList = make([]string, 0)
		} else if err != nil {
			logger.Error(err, "Exec cmd error", "podname", podName)
			return ctrl.Result{}, err
		}
		if len(stdout) == 0 {
			fileList = make([]string, 0)
		} else {
			// filter "." and ".."
			fullList := strings.Split(strings.Trim(stdout, "\n "), "\n")
			fileList = make([]string, 0, len(fullList)-2)
			for _, file := range fullList {
				if file != "." && file != ".." {
					fileList = append(fileList, file)
				}
			}
		}
		// Find not existing file
		curFileSet := make(map[string]bool)
		for _, existingFile := range fileList {
			curFileSet[existingFile] = true
		}
		filesToCreate := make([]string, 0)
		for _, targetFile := range dirFile.Spec.Files {
			if !curFileSet[targetFile] {
				// targetFile not exists, create one
				filesToCreate = append(filesToCreate, targetFile)
			}
		}
		if len(filesToCreate) > 0 {
			logger.Info("creating files", "filesToCreate", filesToCreate, "node", nodeName)
			cmdSlice := []string{"touch"}
			for _, file := range filesToCreate {
				cmdSlice = append(cmdSlice, filepath.Join(dirPath, file))
			}
			_, fileCreateStderr, fileCreateErr := r.podExec(podName, podNamespace, cmdSlice)
			if fileCreateErr != nil {
				logger.Error(fileCreateErr, "create file error", "filesToCreate", filesToCreate, "node", nodeName, "stderr", fileCreateStderr)
			} else {
				fileList = append(fileList, filesToCreate...)
			}
		}
		targetStatus[nodeName] = fileList
	}
	dirFile.Status.ExistingFiles = targetStatus
	if err := r.Status().Update(ctx, &dirFile); err != nil {
		logger.Error(err, "unable to update DirFile status")
	}

	// return ctrl.Result{}, nil
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DirFileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&filev1.DirFile{}).
		Complete(r)
}
