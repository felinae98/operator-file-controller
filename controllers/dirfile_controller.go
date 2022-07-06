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

// Execute given cmd in pod with podName and namespace, returns (stdout, stderr, error)
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

// Get DirFile Object of ctrl.Request
func (r *DirFileReconciler) getDirFile(ctx context.Context, req ctrl.Request) (*filev1.DirFile, error) {
	var dirFile filev1.DirFile
	if err := r.Get(ctx, req.NamespacedName, &dirFile); err != nil {
		return nil, err
	}
	return &dirFile, nil
}

// Get PodList of DaemonSet
func (r *DirFileReconciler) getPodList(ctx context.Context) (*corev1.PodList, error) {
	var podList corev1.PodList
	// namespace "file-keeper" is hard-encoded
	if err := r.List(ctx, &podList, client.InNamespace(podNamespace), client.MatchingLabels{"name": podNameLabel}); err != nil {
		return nil, err
	}
	return &podList, nil
}

// Return files in given directory, create the directory if it does not exist
func (r *DirFileReconciler) createDirAndListFile(logger logr.Logger, nodeName, podName, podNamespace, dirPath string) ([]string, error) {
	stdout, stderr, err := r.podExec(podName, podNamespace, []string{"ls", "-a", dirPath})
	var fileList []string
	if err != nil && strings.Contains(stderr, "No such file or directory") {
		// Dir does not exist
		logger.Info("Dir does not exist, creating", "dirPath", dirPath, "node", nodeName)
		_, dirCreationStderr, dirCreationErr := r.podExec(podName, podNamespace, []string{"mkdir", "-p", dirPath})
		if dirCreationErr != nil {
			logger.Error(dirCreationErr, "failed to create directory", "dirPath", dirPath, "stderr", dirCreationStderr, "node", nodeName)
			return nil, dirCreationErr
		}
		fileList = make([]string, 0)
		return fileList, nil
	} else if err != nil {
		// Query file error
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

func (r *DirFileReconciler) listFileInDir(logger logr.Logger, nodeName, podName, podNamespace, dirPath string) ([]string, error) {
	stdout, stderr, err := r.podExec(podName, podNamespace, []string{"ls", "-a", dirPath})
	var fileList []string
	if err != nil && strings.Contains(stderr, "No such file or directory") {
		// Dir does not exist
		logger.Info("Dir does not exist, creating", "dirPath", dirPath, "node", nodeName)
		_, dirCreationStderr, dirCreationErr := r.podExec(podName, podNamespace, []string{"mkdir", "-p", dirPath})
		if dirCreationErr != nil {
			logger.Error(dirCreationErr, "failed to create directory", "dirPath", dirPath, "stderr", dirCreationStderr, "node", nodeName)
			return nil, dirCreationErr
		}
		fileList = make([]string, 0)
		return fileList, nil
	} else if err != nil {
		// Query file error
		logger.Error(err, "Exec cmd error", "podname", podName)
		return nil, err
	}
}

// Compare file list in DirFile Spec with current file in directory, returns list of file to create
func (r *DirFileReconciler) getFilesToCreate(targetFileList, currentFileList []string) []string {
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
func (r *DirFileReconciler) touchFiles(logger logr.Logger, nodeName, podName, podNamespace, dirPath string, createFileList []string) error {
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

//+kubebuilder:rbac:groups=file.felinae98.cn,resources=dirfiles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=file.felinae98.cn,resources=dirfiles/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=file.felinae98.cn,resources=dirfiles/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pod,verbs=get;list;patch;delete
//+kubebuilder:rbac:groups=core,resources=pod/exec,verbs=create

func (r *DirFileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconcile triggered", "req", req)

	// Get corresponding DirFile Object, exist when no object was found
	dirFile, err := r.getDirFile(ctx, req)
	if err != nil {
		// DirFile object does not exist
		logger.Error(err, "DirFile not found", "Name", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	podList, err := r.getPodList(ctx)
	if err != nil || len(podList.Items) == 0 {
		logger.Error(err, "Pod in namespace with label not found", "PodNamespace", podNamespace, "PodNameLabel", podNameLabel)
		return ctrl.Result{}, err
	}

	targetStatus := make(map[string][]string) // new DirFile Status to update

	for _, pod := range podList.Items {
		nodeName := pod.Spec.NodeName
		podName := pod.Name
		dirPath := filepath.Join(podMountPoint, dirFile.Spec.Dir)

		fileList, err := r.createDirAndListFile(logger, nodeName, podName, podNamespace, dirPath)
		if err != nil {
			return ctrl.Result{}, err
		}

		filesToCreate := r.getFilesToCreate(dirFile.Spec.Files, fileList)
		if len(filesToCreate) > 0 {
			logger.Info("creating files", "filesToCreate", filesToCreate, "node", nodeName)
			err := r.touchFiles(logger, nodeName, podName, podNamespace, dirPath, filesToCreate)
			if err != nil {
				// error when create files, re-get files in dirPath, update Status
				fileList, _ = r.createDirAndListFile(logger, nodeName, podName, podNamespace, dirPath)
			} else {
				// files created successfully
				fileList = append(fileList, filesToCreate...)
			}
		}

		targetStatus[nodeName] = fileList
	}

	dirFile.Status.ExistingFiles = targetStatus
	if err := r.Status().Update(ctx, dirFile); err != nil {
		logger.Error(err, "unable to update DirFile status")
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DirFileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&filev1.DirFile{}).
		Complete(r)
}
