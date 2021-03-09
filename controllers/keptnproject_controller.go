/*


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
	"context"
	"encoding/json"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keptnv1 "keptn-operator/api/v1"
)

type GitCredentials struct {
	User      string `json:"user,omitempty"`
	Token     string `json:"token,omitempty"`
	RemoteURI string `json:"remoteURI,omitempty"`
}

type KeptnConfig struct {
	Metadata KeptnConfigMeta `yaml:"metadata,omitempty"`
	Services []KeptnService  `yaml:"services,omitempty"`
}

type KeptnConfigMeta struct {
	Branch string `yaml:"initbranch,omitempty"`
}

type KeptnService struct {
	Name              string `yaml:"name,omitempty"`
	DeploymentTrigger string `yaml:"triggerevent"`
}

type KeptnTriggerEvent struct {
	ContentType string         `json:"contenttype,omitempty"`
	Data        KeptnEventData `json:"data,omitempty"`
	Source      string         `json:"source,omitempty"`
	SpecVersion string         `json:"specversion,omitempty"`
	Type        string         `json:"type,omitempty"`
}

type KeptnEventData struct {
	Project string `json:"project,omitempty"`
	Service string `json:"service,omitempty"`
	Stage   string `json:"stage,omitempty"`
}

// KeptnProjectReconciler reconciles a KeptnProject object
type KeptnProjectReconciler struct {
	client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	ReqLogger logr.Logger
}

// +kubebuilder:rbac:groups=keptn.operator.keptn.sh,resources=keptnprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keptn.operator.keptn.sh,resources=keptnprojects/status,verbs=get;update;patch

func (r *KeptnProjectReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("keptnproject", req.NamespacedName)

	r.ReqLogger = r.Log.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name)
	r.ReqLogger.Info("Reconciling KeptnProject")

	project := &keptnv1.KeptnProject{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, project)
	if errors.IsNotFound(err) {
		r.ReqLogger.Info("KeptnProject resource not found. Ignoring since object must be deleted")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	secret := &corev1.Secret{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: "git-credentials-" + project.Name, Namespace: req.Namespace}, secret)

	var credentials GitCredentials
	err = json.Unmarshal(secret.Data["git-credentials"], &credentials)
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	mainHead, err := r.getCommitHash(credentials, "")
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	config := &KeptnConfig{}

	// Save new git hashes, if changed

	// GET Configuration
	dir, _ := ioutil.TempDir("", "temp_dir")

	_, err = git.PlainClone(dir, false, &git.CloneOptions{
		URL: credentials.RemoteURI,
		Auth: &http.BasicAuth{
			Username: credentials.User,
			Password: credentials.Token,
		},
		SingleBranch: true,
	})
	if err != nil {
		r.ReqLogger.Error(err, "Could not checkout "+project.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if _, err := os.Stat(filepath.Join(dir, ".keptn/config.yaml")); err == nil {
		yamlFile, err := ioutil.ReadFile(filepath.Join(dir, ".keptn/config.yaml"))
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		err = yaml.Unmarshal(yamlFile, config)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}

		project.Status.WatchedBranch = config.Metadata.Branch

		for _, service := range config.Services {
			err = r.createKeptnService(project.Name, service, req.Namespace)
			if err != nil {
				r.ReqLogger.Error(err, "Could not create service "+project.Name+"/"+service.Name)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, err
			}
		}
	} else {
		r.ReqLogger.Info("There is no configuration file for project " + project.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	defer os.RemoveAll(dir)

	for _, service := range r.getKeptnServices().Items {
		found := false
		for _, configService := range config.Services {
			if service.Spec.Project == project.Name && service.Spec.Service == configService.Name {
				found = true
			}
		}
		if found == false {
			err = r.removeService(project.Name, service.Spec.Service, req.Namespace)
			if err != nil {
				r.ReqLogger.Error(err, "Could not remove Service "+service.Spec.Service)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, err
			}

		}
	}

	project.Status.LastMainCommit = mainHead

	if project.Status.WatchedBranch != "" {
		appCommitHash, err := r.getCommitHash(credentials, project.Status.WatchedBranch)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}

		if project.Status.LastDeployCommit != appCommitHash {
			r.ReqLogger.Info("App Branch State has changed - Triggering new Deployment")
			for _, service := range config.Services {
				err := r.triggerDeployment(project.Name, service, config.Metadata.Branch, req.Namespace)
				if err != nil {
					r.ReqLogger.Error(err, "Could not trigger deployment "+service.Name)
					return ctrl.Result{RequeueAfter: 30 * time.Second}, err
				}
			}
			project.Status.LastDeployCommit = appCommitHash
		}
	}

	err = r.Client.Update(context.TODO(), project)
	if err != nil {
		r.ReqLogger.Error(err, "Could not update LastAppCommit")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	r.ReqLogger.Info("Finished Reconciling")

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *KeptnProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keptnv1.KeptnProject{}).
		Complete(r)
}

func (r *KeptnProjectReconciler) createKeptnService(project string, service KeptnService, namespace string) error {
	currentKService := keptnv1.KeptnService{}
	kService := keptnv1.KeptnService{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      project + "-" + service.Name,
		},
		Spec: keptnv1.KeptnServiceSpec{
			Project:        project,
			Service:        service.Name,
			TriggerCommand: service.DeploymentTrigger,
		},
	}

	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: project + "-" + service.Name, Namespace: namespace}, &currentKService); err != nil && errors.IsNotFound(err) {
		log.Println("Creating a new " + service.Name + "Service")
		err = r.Client.Create(context.TODO(), &kService)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *KeptnProjectReconciler) triggerDeployment(project string, service KeptnService, stage string, namespace string) error {

	keptnService := keptnv1.KeptnService{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: project + "-" + service.Name, Namespace: namespace}, &keptnService)

	keptnService.Status.DeploymentPending = true
	keptnService.Spec.StartStage = stage

	err = r.Client.Update(context.TODO(), &keptnService)
	if err != nil {
		r.ReqLogger.Error(err, "Could not update KeptnService "+service.Name)
		return err
	} else {
		r.ReqLogger.Info("Updated Service")
	}
	return nil
}

func (r *KeptnProjectReconciler) removeService(project string, service string, namespace string) error {

	keptnService := keptnv1.KeptnService{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: project + "-" + service, Namespace: namespace}, &keptnService)

	if keptnService.Status.SafeToDelete == true {
		err = r.Client.Delete(context.TODO(), &keptnService)
		if err != nil {
			r.ReqLogger.Error(err, "Deletion of "+keptnService.Name+" was unsuccessful")
			return err
		} else {
			r.ReqLogger.Info("Deletion of " + keptnService.Name + " was successful")
			return nil
		}
	}

	keptnService.Status.DeletionPending = true
	err = r.Client.Update(context.TODO(), &keptnService)
	if err != nil {
		r.ReqLogger.Error(err, "Could not update KeptnService "+keptnService.Name)
		return err
	} else {
		r.ReqLogger.Info("Updated Service " + keptnService.Name)
	}
	return nil
}

func (r *KeptnProjectReconciler) getCommitHash(credentials GitCredentials, branch string) (string, error) {

	authentication := &http.BasicAuth{
		Username: credentials.User,
		Password: credentials.Token,
	}

	cloneOptions := git.CloneOptions{
		URL:  credentials.RemoteURI,
		Auth: authentication,
	}

	if branch != "" {
		cloneOptions = git.CloneOptions{
			URL:           credentials.RemoteURI,
			Auth:          authentication,
			ReferenceName: plumbing.ReferenceName("refs/heads/" + branch),
		}
	}

	repo, err := git.Clone(memory.NewStorage(), nil, &cloneOptions)
	if err != nil {
		r.ReqLogger.Error(err, "Could not clone repository "+credentials.RemoteURI)
		return "", err
	}

	head, err := repo.Head()
	if err != nil {
		r.ReqLogger.Error(err, "Could get head for "+credentials.RemoteURI)
		return "", err
	}
	return head.Hash().String(), nil
}

func (r *KeptnProjectReconciler) getKeptnServices() keptnv1.KeptnServiceList {
	var keptnServiceList keptnv1.KeptnServiceList

	err := r.Client.List(context.TODO(), &keptnServiceList)
	if err != nil {
		r.ReqLogger.Error(err, "Could not get keptn services")
		return keptnServiceList
	}
	return keptnServiceList
}
