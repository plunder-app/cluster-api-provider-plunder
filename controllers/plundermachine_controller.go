/*
Copyright 2019 The Kubernetes Authors.

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
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha2"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/plunder-app/cluster-api-provider-plunder/api/v1alpha1"
	"github.com/plunder-app/plunder/pkg/apiserver"
	"github.com/plunder-app/plunder/pkg/parlay/parlaytypes"
	"github.com/plunder-app/plunder/pkg/plunderlogging"
	"github.com/plunder-app/plunder/pkg/services"
)

// PlunderMachineReconciler reconciles a PlunderMachine object
type PlunderMachineReconciler struct {
	client.Client
	Log logr.Logger
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=plundermachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=plundermachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machines;machines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events;secrets,verbs=get;list;watch;create;update;patch

func (r *PlunderMachineReconciler) Reconcile(req ctrl.Request) (_ ctrl.Result, rerr error) {
	ctx := context.Background()
	log := r.Log.WithValues("plundermachine", req.NamespacedName)

	// your Plunder Machine logic begins here

	// Fetch the inceptionmachine instance.
	plunderMachine := &infrav1.PlunderMachine{}

	err := r.Get(ctx, req.NamespacedName, plunderMachine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log = log.WithName(plunderMachine.APIVersion)

	// Fetch the Machine.
	machine, err := util.GetOwnerMachine(ctx, r.Client, plunderMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Machine Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}
	log = log.WithName(fmt.Sprintf("machine=%s", machine.Name))

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, nil
	}

	log = log.WithName(fmt.Sprintf("cluster=%s", cluster.Name))

	// Fetch the Plunder Cluster
	plunderCluster := &infrav1.PlunderCluster{}
	plunderClusterName := types.NamespacedName{
		Namespace: plunderMachine.Namespace, // get the name from the machine
		Name:      cluster.Spec.InfrastructureRef.Name,
	}

	if err := r.Client.Get(ctx, plunderClusterName, plunderCluster); err != nil {
		log.Info("The Plunder Cluster is not available yet")
		return ctrl.Result{}, nil
	}

	log = log.WithName(fmt.Sprintf("plunderCluster=%s", plunderCluster.Name))

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(plunderMachine, r)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Always attempt to Patch the PlunderMachine object and status after each reconciliation.
	defer func() {
		if err := patchHelper.Patch(ctx, plunderMachine); err != nil {
			log.Error(err, "failed to patch PlunderMachine")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	// Handle deleted clusters
	if !plunderMachine.DeletionTimestamp.IsZero() {
		return r.reconcileMachineDelete(log, machine, plunderMachine, cluster, plunderCluster)
	}

	// Handle non-deleted clusters
	return r.reconcileMachine(log, machine, plunderMachine, cluster, plunderCluster)
}

func (r *PlunderMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.PlunderMachine{}).
		Complete(r)
}

func (r *PlunderMachineReconciler) reconcileMachine(log logr.Logger, machine *clusterv1.Machine, plunderMachine *infrav1.PlunderMachine, cluster *clusterv1.Cluster, plunderCluster *infrav1.PlunderCluster) (_ ctrl.Result, reterr error) {
	log.Info("Reconciling Machine")
	// If the DockerMachine doesn't have finalizer, add it.
	if !util.Contains(plunderMachine.Finalizers, infrav1.MachineFinalizer) {
		plunderMachine.Finalizers = append(plunderMachine.Finalizers, infrav1.MachineFinalizer)
	}

	// Immeditaly give it the details it needs
	//	providerID := "inception:////inception"

	// if the machine is already provisioned, return
	if plunderMachine.Spec.ProviderID != nil {
		plunderMachine.Status.Ready = true

		return ctrl.Result{}, nil
	}

	// Make sure bootstrap data is available and populated.
	if machine.Spec.Bootstrap.Data == nil {
		log.Info("The Plunder Provider currently doesn't require bootstrap data")
	}

	var installMAC string

	// Find a machine for provisioning
	u, c, err := apiserver.BuildEnvironmentFromConfig("plunderclient.yaml", "")
	if err != nil {
		return ctrl.Result{}, err
	}
	ep, resp := apiserver.FindFunctionEndpoint(u, c, "dhcp", http.MethodGet)
	if resp.Error != "" {
		return ctrl.Result{}, fmt.Errorf(resp.Error)

	}

	u.Path = path.Join(u.Path, ep.Path+"/unleased")

	response, err := apiserver.ParsePlunderGet(u, c)
	if err != nil {
		return ctrl.Result{}, err
	}
	// If an error has been returned then handle the error gracefully and terminate
	if response.FriendlyError != "" || response.Error != "" {
		return ctrl.Result{}, err
	}
	var unleased []services.Lease

	err = json.Unmarshal(response.Payload, &unleased)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Iterate through all known addresses and find a free one that looks "recent"
	for i := range unleased {
		if time.Since(unleased[i].Expiry).Minutes() < 10 {
			installMAC = unleased[i].Nic
		}
	}

	// Hopefully we found one!
	if installMAC == "" {
		return ctrl.Result{}, fmt.Errorf("No free hardware for provisioning")
	}

	log.Info(fmt.Sprintf("Found Hardware %s", installMAC))

	d := services.DeploymentConfig{
		ConfigName: "preseed",
		MAC:        installMAC,
		ConfigHost: services.HostConfig{
			IPAddress: "192.168.1.123",
		},
	}

	//Check the role of the machine
	if util.IsControlPlaneMachine(machine) {
		log.Info(fmt.Sprintf("Provisioning Control plane node %s", machine.Name))
		d.ConfigHost.ServerName = fmt.Sprintf("controlplane-%s", StringWithCharset(5, charset))

	} else {
		log.Info(fmt.Sprintf("Provisioning Worker node %s", machine.Name))
		d.ConfigHost.ServerName = fmt.Sprintf("worker-%s", StringWithCharset(5, charset))
	}

	ep, resp = apiserver.FindFunctionEndpoint(u, c, "deployment", http.MethodPost)
	if resp.Error != "" {
		return ctrl.Result{}, fmt.Errorf(resp.Error)

	}

	u.Path = ep.Path

	b, err := json.Marshal(d)
	if err != nil {
		return ctrl.Result{}, err
	}
	response, err = apiserver.ParsePlunderPost(u, c, b)
	if err != nil {
		return ctrl.Result{}, err
	}
	// If an error has been returned then handle the error gracefully and terminate
	if response.FriendlyError != "" || response.Error != "" {
		return ctrl.Result{}, fmt.Errorf(resp.Error)

	}

	newMap := uptimeCommand(d.ConfigHost.IPAddress)

	// Marshall the parlay submission (runs the uptime command)
	b, err = json.Marshal(newMap)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Create the string that will be used to get the logs
	dashAddress := strings.Replace(d.ConfigHost.IPAddress, ".", "-", -1)

	// Get the time
	t := time.Now()

	for {
		// Set Parlay API path and POST
		ep, resp = apiserver.FindFunctionEndpoint(u, c, "parlay", http.MethodPost)
		if resp.Error != "" {
			return ctrl.Result{}, fmt.Errorf(resp.Error)

		}
		u.Path = ep.Path

		response, err := apiserver.ParsePlunderPost(u, c, b)
		if err != nil {
			return ctrl.Result{}, err
		}

		// If an error has been returned then handle the error gracefully and terminate
		if response.FriendlyError != "" || response.Error != "" {
			return ctrl.Result{}, fmt.Errorf(resp.Error)

		}

		// Sleep for five seconds
		time.Sleep(5 * time.Second)

		// Set the parlay API get logs path and GET
		ep, resp = apiserver.FindFunctionEndpoint(u, c, "parlayLog", http.MethodGet)
		if resp.Error != "" {
			return ctrl.Result{}, fmt.Errorf(resp.Error)

		}
		u.Path = ep.Path + "/" + dashAddress

		response, err = apiserver.ParsePlunderGet(u, c)
		if err != nil {
			return ctrl.Result{}, err
		}
		// If an error has been returned then handle the error gracefully and terminate
		if response.FriendlyError != "" || response.Error != "" {
			return ctrl.Result{}, fmt.Errorf(resp.Error)

		}

		var logs plunderlogging.JSONLog

		err = json.Unmarshal(response.Payload, &logs)
		if err != nil {
			return ctrl.Result{}, err
		}

		if logs.State == "Completed" {
			log.Info(fmt.Sprintf("Host has been succesfully provisioned OS in %s Seconds\n", time.Since(t).Round(time.Second)))
			break
		}
	}

	// TODO - Attempt to create the machine

	// // if the machine is a control plane added, update the load balancer configuration
	// if util.IsControlPlaneMachine(machine) {}

	// DEPLOY THE MACHINE
	//clusterDeploy(nil)

	providerID := fmt.Sprintf("plunder://%s", installMAC)

	plunderMachine.Spec.ProviderID = &providerID
	// Mark the inceptionMachine ready
	plunderMachine.Status.Ready = true

	return ctrl.Result{}, nil

}

func (r *PlunderMachineReconciler) reconcileMachineDelete(logger logr.Logger, machine *clusterv1.Machine, inceptionMachine *infrav1.PlunderMachine, cluster *clusterv1.Cluster, inceptionCluster *infrav1.PlunderCluster) (_ ctrl.Result, reterr error) {
	logger.Info("Deleting Machine")
	// Machine is deleted so remove the finalizer.
	inceptionMachine.Finalizers = util.Filter(inceptionMachine.Finalizers, infrav1.MachineFinalizer)
	return ctrl.Result{}, nil

}

func uptimeCommand(host string) parlaytypes.TreasureMap {
	return parlaytypes.TreasureMap{
		Deployments: []parlaytypes.Deployment{
			parlaytypes.Deployment{
				Name:     "Cluster-API provisioning",
				Parallel: false,
				Hosts:    []string{host},
				Actions: []parlaytypes.Action{
					parlaytypes.Action{
						ActionType: "command",
						Command:    "uptime",
						Name:       "Cluster-API provisioning uptime command",
					},
				},
			},
		},
	}
}
