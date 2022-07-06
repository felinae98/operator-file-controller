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

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
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
	podNamespace     = "file-keeper"
	podNameLabel     = "file-loader-pod"
	podContainerName = "file-loader-pod"
	podMountPoint    = "/mnt"
)

// FileKeeperReconciler reconciles a FileKeeper object
type FileKeeperReconciler struct {
	client.Client
	RESTClient rest.Interface
	RESTConfig *rest.Config
	Scheme     *runtime.Scheme
}

// Execute given cmd in pod with podName and namespace, returns (stdout, stderr, error)
func (r *FileKeeperReconciler) podExec(podName string, namespace string, cmd []string) (string, string, error) {
	execReq := r.RESTClient.Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: podContainerName,
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

// List file name in given dirPath, returns slice of file name and error
func (r *FileKeeperReconciler) listFileInDir(logger logr.Logger, nodeName, podName, podNamespace, dirPath string) ([]string, error) {
	stdout, stderr, err := r.podExec(podName, podNamespace, []string{"ls", "-a", dirPath})
	var fileList []string
	if err != nil && strings.Contains(stderr, "No such file or directory") {
		// Dir does not exist
		return nil, errors.New("Directory not found")
	} else if err != nil {
		err = errors.WithMessagef(err, "ls files error, stderr: %s", stderr)
		logger.Error(err, "Exec cmd error", "podname", podName)
		return nil, err
	}
	// filter out "." and "..", because `ls -a` returns "." and  ".." which are not files
	fullList := strings.Split(strings.Trim(stdout, "\n "), "\n")
	fileList = make([]string, 0, len(fullList)-2)
	for _, file := range fullList {
		if file != "." && file != ".." {
			fileList = append(fileList, file)
		}
	}
	return fileList, nil
}

func (r *FileKeeperReconciler) createDir(logger logr.Logger, nodeName, podName, podNamespace, dirPath string) error {
	_, stderr, err := r.podExec(podName, podNamespace, []string{"mkdir", "-p", dirPath})
	if err != nil {
		err = errors.WithMessagef(err, "Directory creation failed, stderr: %s", stderr)
		logger.Error(err, "failed to create directory", "dirPath", dirPath, "node", nodeName)
		return err
	}
	return nil
}

// Compare file list in FileKeeper Spec with current file in directory, returns list of file to create
func (r *FileKeeperReconciler) getFilesToCreate(targetFileList, currentFileList []string) []string {
	curFileSet := make(map[string]bool)
	for _, existingFile := range currentFileList {
		curFileSet[existingFile] = true
	}
	filesToCreate := make([]string, 0)
	for _, targetFile := range targetFileList {
		if !curFileSet[targetFile] {
			// targetFile not exists, create one
			filesToCreate = append(filesToCreate, targetFile)
		}
	}
	return filesToCreate
}

// Touch files in dirPath, returns err
func (r *FileKeeperReconciler) batchTouchFiles(logger logr.Logger, nodeName, podName, podNamespace, dirPath string, createFileList []string) error {
	cmdSlice := []string{"touch"}
	for _, file := range createFileList {
		cmdSlice = append(cmdSlice, filepath.Join(dirPath, file))
	}

	_, fileCreateStderr, fileCreateErr := r.podExec(podName, podNamespace, cmdSlice)
	if fileCreateErr != nil {
		logger.Error(fileCreateErr, "create file error", "createFileList", createFileList, "node", nodeName, "stderr", fileCreateStderr)
		return fileCreateErr
	} else {
		return nil
	}
}

//+kubebuilder:rbac:groups=file.felinae98.cn,resources=filekeepers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=file.felinae98.cn,resources=filekeepers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=file.felinae98.cn,resources=filekeepers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pod,verbs=get;list;patch;delete
//+kubebuilder:rbac:groups=core,resources=pod/exec,verbs=create

func (r *FileKeeperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconcile triggered", "req", req)

	// Get corresponding FileKeeper Object, exit when no object was found
	var fileKeeper filev1.FileKeeper
	if err := r.Get(ctx, req.NamespacedName, &fileKeeper); err != nil {
		logger.Error(err, "FileKeeper not found", "Name", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Get PodList used by operator, exit when required pods do not exist
	var podList corev1.PodList
	err := r.List(ctx, &podList, client.InNamespace(podNamespace), client.MatchingLabels{"name": podNameLabel})
	if err != nil || len(podList.Items) == 0 {
		logger.Error(err, "Pod in namespace with label not found", "PodNamespace", podNamespace, "PodNameLabel", podNameLabel)
		return ctrl.Result{}, err
	}

	targetStatus := make(map[string][]string) // new FileKeeper Status to update

	for _, pod := range podList.Items {
		nodeName := pod.Spec.NodeName
		podName := pod.Name
		dirPath := filepath.Join(podMountPoint, fileKeeper.Spec.Dir)

		fileList, err := r.listFileInDir(logger, nodeName, podName, podNamespace, dirPath)
		if err != nil && err.Error() == "Directory not found" {
			logger.Info("Dir does not exist, creating", "dirPath", dirPath, "node", nodeName)
			err = r.createDir(logger, nodeName, podName, podNamespace, dirPath)
			if err != nil {
				return ctrl.Result{}, err
			}
			fileList = make([]string, 0) // new directory should be empty
		} else if err != nil {
			return ctrl.Result{}, err
		}

		filesToCreate := r.getFilesToCreate(fileKeeper.Spec.Files, fileList)
		if len(filesToCreate) > 0 {
			logger.Info("creating files", "filesToCreate", filesToCreate, "node", nodeName)
			err := r.batchTouchFiles(logger, nodeName, podName, podNamespace, dirPath, filesToCreate)
			if err != nil {
				logger.Error(err, "Unable to create files")
			} else {
				fileList = append(fileList, filesToCreate...)
			}
		}

		targetStatus[nodeName] = fileList
	}

	fileKeeper.Status.ExistingFiles = targetStatus
	if err := r.Status().Update(ctx, &fileKeeper); err != nil {
		logger.Error(err, "unable to update FileKeeper status")
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FileKeeperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&filev1.FileKeeper{}).
		Complete(r)
}
