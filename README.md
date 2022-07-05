# file-controller
An operator to make sure a given list of file in given directory exists (create them when not exists).

## Description
This operator watches `dirfiles.files.felinae98.cn` resource, and track files in `dir` and create file in `files`. For example,
```yaml
apiVersion: file.felinae98.cn/v1
kind: DirFile
metadata:
  name: dirfile-sample
spec:
  dir: /tmp/a
  files:
    - file1
    - file4
```
the operator will track files in `/tmp/a`, and make sure `/tmp/a/file1` and `/tmp/a/file2` exists. And the status of this resource shows the files in `/tmp/a` of each node.

Assume that you have a cluster with two nodes, the `status` of `dirfile` resource will show as follows:
```yaml
Status:
  Exsiting Files:
    k8s-node1: # <- node name
      file1 # <- file name of files in `/tmp/a` in k8s-node1
      file4
    k8s-node2:
      file1
      file4
      t1 # additional file in k8s-node2
```

## Getting Started
You’ll need a Kubernetes cluster to run against. You can use [KIND](https://sigs.k8s.io/kind) to get a local cluster for testing, or run against a remote cluster.
**Note:** Your controller will automatically use the current context in your kubeconfig file (i.e. whatever cluster `kubectl cluster-info` shows).

### Basic Usage

1. Install DaemonSet

**Note**: The DaemonSet is not controlled by operator, you must make sure the DaemonSet was applied before install instances of cr.

```sh
kubectl apply -f daemonset.yaml
```

2. Install Custom Resource Definition

```sh
make install
```

3. Run Operator Locally

```sh
make run
```

4. Install Instances of Custom Resources:

```sh
kubectl apply -f config/samples/
```

### Publish or Deploy to Cluster

Please refer to kubebuiler documentation for further information.

### How it works
This project aims to follow the Kubernetes [Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)

It uses [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/) 
which provides a reconcile function responsible for synchronizing resources untile the desired state is reached on the cluster 

### Modifying the API definitions
If you are editing the API definitions, generate the manifests such as CRs or CRDs using:

```sh
make manifests
```

**NOTE:** Run `make --help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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

