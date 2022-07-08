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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	filev1 "felinae98.cn/file-controller/api/v1"
)

const (
	podMountPoint    = "/mnt"
)

var dirNotExists = errors.New("directory does not exist")

// FileKeeperReconciler reconciles a FileKeeper object
type FileKeeperReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	NodeName   string
}

// Execute cmd in **current** pod, returns (stdout, stderr, error)
func (r *FileKeeperReconciler) exec(ctx context.Context, cmd []string) (string, string, error) {
	name := cmd[0]
	args := cmd[1:]
	cmdObj := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmdObj.Stdout = &stdout
	cmdObj.Stderr = &stderr
	err := cmdObj.Run()
	return stdout.String(), stderr.String(), err
}

// List file name in given dirPath, returns slice of file name and error
func (r *FileKeeperReconciler) listFileInDir(ctx context.Context, logger logr.Logger, dirPath string) ([]string, error) {
	stdout, stderr, err := r.exec(ctx, []string{"ls", "-a", dirPath})
	var fileList []string
	if err != nil && strings.Contains(stderr, "No such file or directory") {
		// Dir does not exist
		return nil, dirNotExists
	} else if err != nil {
		err = errors.WithMessagef(err, "ls files error, stderr: %s", stderr)
		logger.Error(err, "Exec cmd error", "nodeName", r.NodeName)
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

func (r *FileKeeperReconciler) createDir(ctx context.Context, logger logr.Logger, dirPath string) error {
	_, stderr, err := r.exec(ctx, []string{"mkdir", "-p", dirPath})
	if err != nil {
		err = errors.WithMessagef(err, "Directory creation failed, stderr: %s", stderr)
		logger.Error(err, "failed to create directory", "dirPath", dirPath, "node", r.NodeName)
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
func (r *FileKeeperReconciler) batchTouchFiles(ctx context.Context, logger logr.Logger, dirPath string, createFileList []string) error {
	cmdSlice := []string{"touch"}
	for _, file := range createFileList {
		cmdSlice = append(cmdSlice, filepath.Join(dirPath, file))
	}

	_, stderr, err := r.exec(ctx, cmdSlice)
	if err != nil {
		logger.Error(err, "create file error", "createFileList", createFileList, "node", r.NodeName, "stderr", stderr)
		return err
	} else {
		return nil
	}
}

// Update Status.ExistingFiles for nodeName.
// Because multi operator update cr status simultaneously, and every operator update Status.ExistingFiles[node name of operator]
// use k8s json merge patch is enough to avoid updating conflict
func (r *FileKeeperReconciler) updateNodeStatus(ctx context.Context, originFileKeeper *filev1.FileKeeper, nodeName string, files []string) error {
	newFileKeeper := originFileKeeper.DeepCopy()
	if newFileKeeper.Status.ExistingFiles == nil {
		newFileKeeper.Status.ExistingFiles = make(map[string][]string)
	}
	newFileKeeper.Status.ExistingFiles[nodeName] = files
	patch := client.MergeFrom(originFileKeeper)
	err := r.Status().Patch(ctx, newFileKeeper, patch)
	return err
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

	dirPath := filepath.Join(podMountPoint, fileKeeper.Spec.Dir)

	fileList, err := r.listFileInDir(ctx, logger, dirPath)
	if err != nil && errors.Is(err, dirNotExists) {
		logger.Info("Dir does not exist, creating", "dirPath", dirPath, "node", r.NodeName)
		err = r.createDir(ctx, logger, dirPath)
		if err != nil {
			return ctrl.Result{}, err
		}
		fileList = make([]string, 0) // new directory should be empty
	} else if err != nil {
		return ctrl.Result{}, err
	}

	filesToCreate := r.getFilesToCreate(fileKeeper.Spec.Files, fileList)
	if len(filesToCreate) > 0 {
		logger.Info("creating files", "filesToCreate", filesToCreate, "node", r.NodeName)
		err := r.batchTouchFiles(ctx, logger, dirPath, filesToCreate)
		if err != nil {
			logger.Error(err, "Unable to create files")
		} else {
			fileList = append(fileList, filesToCreate...)
		}
	}

	if err := r.updateNodeStatus(ctx, &fileKeeper, r.NodeName, fileList); err != nil {
		logger.Error(err, "Unable to patch status", "nodeName", r.NodeName)
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FileKeeperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&filev1.FileKeeper{}).
		Complete(r)
}
