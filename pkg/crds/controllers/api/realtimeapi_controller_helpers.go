/*
Copyright 2021 Cortex Labs, Inc.

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

package api

import (
	"context"
	"fmt"

	"github.com/cortexlabs/cortex/pkg/consts"
	apiv1alpha1 "github.com/cortexlabs/cortex/pkg/crds/apis/api/v1alpha1"
	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/k8s"
	"github.com/cortexlabs/cortex/pkg/lib/pointer"
	"github.com/cortexlabs/cortex/pkg/lib/strings"
	"github.com/cortexlabs/cortex/pkg/types/status"
	"github.com/cortexlabs/cortex/pkg/types/userconfig"
	"github.com/cortexlabs/cortex/pkg/workloads"
	istionetworking "istio.io/api/networking/v1beta1"
	istioclientnetworking "istio.io/client-go/pkg/apis/networking/v1beta1"
	kapps "k8s.io/api/apps/v1"
	kcore "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	kresource "k8s.io/apimachinery/pkg/api/resource"
	kmeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *RealtimeAPIReconciler) getDeployment(ctx context.Context, api apiv1alpha1.RealtimeAPI) (*kapps.Deployment, error) {
	req := client.ObjectKey{Namespace: api.Namespace, Name: workloads.K8sName(api.Name)}
	deployment := kapps.Deployment{}
	if err := r.Get(ctx, req, &deployment); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &deployment, nil
}

func (r *RealtimeAPIReconciler) updateStatus(ctx context.Context, api *apiv1alpha1.RealtimeAPI, deployment *kapps.Deployment) error {
	apiStatus := status.Pending
	api.Status.Status = apiStatus // FIXME: handle other status

	endpoint, err := r.getEndpoint(ctx, api)
	if err != nil {
		return errors.Wrap(err, "failed to get api endpoint")
	}

	api.Status.Endpoint = endpoint
	if deployment != nil {
		api.Status.DesiredReplicas = *deployment.Spec.Replicas
		api.Status.CurrentReplicas = deployment.Status.Replicas
		api.Status.ReadyReplicas = deployment.Status.ReadyReplicas
	}

	if err = r.Status().Update(ctx, api); err != nil {
		return err
	}

	return nil
}

func (r *RealtimeAPIReconciler) createOrUpdateDeployment(ctx context.Context, api apiv1alpha1.RealtimeAPI) (controllerutil.OperationResult, error) {
	deployment := kapps.Deployment{
		ObjectMeta: kmeta.ObjectMeta{
			Name:      workloads.K8sName(api.Name),
			Namespace: api.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r, &deployment, func() error {
		deployment.Spec = r.desiredDeployment(api).Spec
		return nil
	})
	if err != nil {
		return op, err
	}
	return op, nil
}

func (r *RealtimeAPIReconciler) createOrUpdateService(ctx context.Context, api apiv1alpha1.RealtimeAPI) (controllerutil.OperationResult, error) {
	service := kcore.Service{
		ObjectMeta: kmeta.ObjectMeta{
			Name:      workloads.K8sName(api.Name),
			Namespace: api.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r, &service, func() error {
		service.Spec = r.desiredService(api).Spec
		return nil
	})
	if err != nil {
		return op, err
	}
	return op, nil
}

func (r *RealtimeAPIReconciler) createOrUpdateVirtualService(ctx context.Context, api apiv1alpha1.RealtimeAPI) (controllerutil.OperationResult, error) {
	vs := istioclientnetworking.VirtualService{
		ObjectMeta: kmeta.ObjectMeta{
			Name:      workloads.K8sName(api.Name),
			Namespace: api.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r, &vs, func() error {
		vs.Spec = r.desiredVirtualService(api).Spec
		return nil
	})
	if err != nil {
		return op, err
	}
	return op, nil
}

func (r *RealtimeAPIReconciler) getEndpoint(ctx context.Context, api *apiv1alpha1.RealtimeAPI) (string, error) {
	req := client.ObjectKey{Namespace: consts.IstioNamespace, Name: "ingressgateway-apis"}
	svc := kcore.Service{}
	if err := r.Get(ctx, req, &svc); err != nil {
		return "", err
	}

	ingress := svc.Status.LoadBalancer.Ingress
	if ingress == nil || len(ingress) == 0 {
		return "", nil
	}

	endpoint := fmt.Sprintf("http://%s/%s",
		svc.Status.LoadBalancer.Ingress[0].Hostname, api.Spec.Networking.Endpoint,
	)

	return endpoint, nil
}

func (r *RealtimeAPIReconciler) desiredDeployment(api apiv1alpha1.RealtimeAPI) kapps.Deployment {
	containers, volumes := r.desiredContainers(api)

	return *k8s.Deployment(&k8s.DeploymentSpec{
		Name:           workloads.K8sName(api.Name),
		Replicas:       api.Spec.Pod.Replicas,
		MaxSurge:       pointer.String(api.Spec.UpdateStrategy.MaxSurge.String()),
		MaxUnavailable: pointer.String(api.Spec.UpdateStrategy.MaxUnavailable.String()),
		Labels: map[string]string{
			"apiName":        api.Name,
			"apiKind":        userconfig.RealtimeAPIKind.String(),
			"apiID":          api.Annotations["cortex.dev/api-id"],        // TODO: check if can be replaced with resource version
			"deploymentID":   api.Annotations["cortex.dev/deployment-id"], // FIXME: needs to be created beforehand
			"cortex.dev/api": "true",
		},
		Annotations: getAPIAnnotations(api),
		Selector: map[string]string{
			"apiName": api.Name,
			"apiKind": userconfig.RealtimeAPIKind.String(),
		},
		PodSpec: k8s.PodSpec{
			Labels: map[string]string{
				"apiName":        api.Name,
				"apiKind":        userconfig.RealtimeAPIKind.String(),
				"deploymentID":   api.Annotations["cortex.dev/deployment-id"],
				"cortex.dev/api": "true",
			},
			Annotations: map[string]string{
				"traffic.sidecar.istio.io/excludeOutboundIPRanges": "0.0.0.0/0",
			},
			K8sPodSpec: kcore.PodSpec{
				RestartPolicy:                 kcore.RestartPolicyAlways,
				TerminationGracePeriodSeconds: pointer.Int64(_terminationGracePeriodSeconds),
				Containers:                    containers,
				NodeSelector:                  workloads.NodeSelectors(),
				Tolerations:                   workloads.GenerateResourceTolerations(),
				Affinity:                      workloads.GenerateNodeAffinities(api.Spec.NodeGroups),
				Volumes:                       volumes,
				ServiceAccountName:            workloads.ServiceAccountName,
			},
		},
	})
}

func (r *RealtimeAPIReconciler) desiredContainers(api apiv1alpha1.RealtimeAPI) ([]kcore.Container, []kcore.Volume) {
	containers, volumes := r.userContainers(api)
	proxyContainer, proxyVolume := r.proxyContainer(api)

	containers = append(containers, proxyContainer)
	volumes = append(volumes, proxyVolume)

	return containers, volumes
}

func (r *RealtimeAPIReconciler) desiredService(api apiv1alpha1.RealtimeAPI) kcore.Service {
	return *k8s.Service(&k8s.ServiceSpec{
		Name:        workloads.K8sName(api.Name),
		PortName:    "http",
		Port:        consts.ProxyPortInt32,
		TargetPort:  consts.ProxyPortInt32,
		Annotations: getAPIAnnotations(api),
		Labels: map[string]string{
			"apiName":        api.Name,
			"apiKind":        userconfig.RealtimeAPIKind.String(),
			"cortex.dev/api": "true",
		},
		Selector: map[string]string{
			"apiName": api.Name,
			"apiKind": userconfig.RealtimeAPIKind.String(),
		},
	})
}

func (r *RealtimeAPIReconciler) desiredVirtualService(api apiv1alpha1.RealtimeAPI) istioclientnetworking.VirtualService {
	var activatorWeight int32
	if api.Spec.Pod.Replicas == 0 {
		activatorWeight = 100
	}

	return *k8s.VirtualService(&k8s.VirtualServiceSpec{
		Name:     workloads.K8sName(api.Name),
		Gateways: []string{"apis-gateway"},
		Destinations: []k8s.Destination{
			{
				ServiceName: workloads.K8sName(api.Name),
				Weight:      100 - activatorWeight,
				Port:        uint32(consts.ProxyPortInt32),
				Headers: &istionetworking.Headers{
					Response: &istionetworking.Headers_HeaderOperations{
						Set: map[string]string{
							consts.CortexOriginHeader: "api",
						},
					},
				},
			},
			{
				ServiceName: consts.ActivatorName,
				Weight:      activatorWeight,
				Port:        uint32(consts.ActivatorPortInt32),
				Headers: &istionetworking.Headers{
					Request: &istionetworking.Headers_HeaderOperations{
						Set: map[string]string{
							consts.CortexAPINameHeader: api.Name,
							consts.CortexTargetServiceHeader: fmt.Sprintf(
								"http://%s.%s:%d",
								workloads.K8sName(api.Name),
								consts.DefaultNamespace,
								consts.ProxyPortInt32,
							),
						},
					},
					Response: &istionetworking.Headers_HeaderOperations{
						Set: map[string]string{
							consts.CortexOriginHeader: consts.ActivatorName,
						},
					},
				},
			},
		},
		PrefixPath:  pointer.String(api.Spec.Networking.Endpoint),
		Rewrite:     pointer.String("/"),
		Annotations: getAPIAnnotations(api),
		Labels: map[string]string{
			"apiName":        api.Name,
			"apiKind":        userconfig.RealtimeAPIKind.String(),
			"apiID":          api.Annotations["cortex.dev/api-id"],
			"deploymentID":   api.Annotations["cortex.dev/deployment-id"],
			"cortex.dev/api": "true",
		},
	})
}

func (r *RealtimeAPIReconciler) userContainers(api apiv1alpha1.RealtimeAPI) ([]kcore.Container, []kcore.Volume) {
	volumes := []kcore.Volume{
		workloads.MntVolume(),
		workloads.CortexVolume(),
		workloads.ClientConfigVolume(),
	}
	containerMounts := []kcore.VolumeMount{
		workloads.MntMount(),
		workloads.CortexMount(),
		workloads.ClientConfigMount(),
	}

	var containers []kcore.Container
	for _, container := range api.Spec.Pod.Containers {
		containerResourceList := kcore.ResourceList{}
		containerResourceLimitsList := kcore.ResourceList{}
		securityContext := kcore.SecurityContext{
			Privileged: pointer.Bool(true),
		}

		if container.Compute.CPU != nil {
			containerResourceList[kcore.ResourceCPU] = *k8s.QuantityPtr(container.Compute.CPU.DeepCopy())
		}

		if container.Compute.Mem != nil {
			containerResourceList[kcore.ResourceMemory] = *k8s.QuantityPtr(container.Compute.Mem.DeepCopy())
		}

		if container.Compute.GPU > 0 {
			containerResourceList["nvidia.com/gpu"] = *kresource.NewQuantity(container.Compute.GPU, kresource.DecimalSI)
			containerResourceLimitsList["nvidia.com/gpu"] = *kresource.NewQuantity(container.Compute.GPU, kresource.DecimalSI)
		}

		if container.Compute.Inf > 0 {
			totalHugePages := container.Compute.Inf * workloads.HugePagesMemPerInf
			containerResourceList["aws.amazon.com/neuron"] = *kresource.NewQuantity(container.Compute.Inf, kresource.DecimalSI)
			containerResourceList["hugepages-2Mi"] = *kresource.NewQuantity(totalHugePages, kresource.BinarySI)
			containerResourceLimitsList["aws.amazon.com/neuron"] = *kresource.NewQuantity(container.Compute.Inf, kresource.DecimalSI)
			containerResourceLimitsList["hugepages-2Mi"] = *kresource.NewQuantity(totalHugePages, kresource.BinarySI)

			securityContext.Capabilities = &kcore.Capabilities{
				Add: []kcore.Capability{
					"SYS_ADMIN",
					"IPC_LOCK",
				},
			}
		}

		if container.Compute.Shm != nil {
			volumes = append(volumes, workloads.ShmVolume(*container.Compute.Shm, "dshm-"+container.Name))
			containerMounts = append(containerMounts, workloads.ShmMount("dshm-"+container.Name))
		}

		containerEnvVars := workloads.BaseEnvVars
		containerEnvVars = append(containerEnvVars, workloads.ClientConfigEnvVar())
		containerEnvVars = append(containerEnvVars, container.Env...)

		containers = append(containers, kcore.Container{
			Name:           container.Name,
			Image:          container.Image,
			Command:        container.Command,
			Args:           container.Args,
			Env:            containerEnvVars,
			VolumeMounts:   containerMounts,
			LivenessProbe:  container.LivenessProbe,
			ReadinessProbe: container.ReadinessProbe,
			Resources: kcore.ResourceRequirements{
				Requests: containerResourceList,
				Limits:   containerResourceLimitsList,
			},
			ImagePullPolicy: kcore.PullAlways,
			SecurityContext: &securityContext,
		})
	}

	return containers, volumes
}

func (r *RealtimeAPIReconciler) proxyContainer(api apiv1alpha1.RealtimeAPI) (kcore.Container, kcore.Volume) {
	return kcore.Container{
		Name:            workloads.ProxyContainerName,
		Image:           r.ClusterConfig.ImageProxy,
		ImagePullPolicy: kcore.PullAlways,
		Args: []string{
			"--cluster-config",
			consts.DefaultInClusterConfigPath,
			"--port",
			consts.ProxyPortStr,
			"--admin-port",
			consts.AdminPortStr,
			"--user-port",
			strings.Int32(api.Spec.Pod.Port),
			"--max-concurrency",
			strings.Int32(api.Spec.Pod.MaxConcurrency),
			"--max-queue-length",
			strings.Int32(api.Spec.Pod.MaxQueueLength),
		},
		Ports: []kcore.ContainerPort{
			{Name: consts.AdminPortName, ContainerPort: consts.AdminPortInt32},
			{ContainerPort: consts.ProxyPortInt32},
		},
		Env:     workloads.BaseEnvVars,
		EnvFrom: workloads.BaseClusterEnvVars(),
		VolumeMounts: []kcore.VolumeMount{
			workloads.ClusterConfigMount(),
		},
		Resources: kcore.ResourceRequirements{
			Requests: kcore.ResourceList{
				kcore.ResourceCPU:    consts.CortexProxyCPU,
				kcore.ResourceMemory: consts.CortexProxyMem,
			},
		},
		ReadinessProbe: &kcore.Probe{
			Handler: kcore.Handler{
				HTTPGet: &kcore.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(int(consts.AdminPortInt32)),
				},
			},
			InitialDelaySeconds: 1,
			TimeoutSeconds:      1,
			PeriodSeconds:       10,
			SuccessThreshold:    1,
			FailureThreshold:    1,
		},
	}, workloads.ClusterConfigVolume()
}

func getAPIAnnotations(api apiv1alpha1.RealtimeAPI) map[string]string {
	return map[string]string{
		userconfig.MinReplicasAnnotationKey:                  strings.Int32(api.Spec.Autoscaling.MinReplicas),
		userconfig.MaxReplicasAnnotationKey:                  strings.Int32(api.Spec.Autoscaling.MaxReplicas),
		userconfig.TargetInFlightAnnotationKey:               strings.Int32(api.Spec.Autoscaling.TargetInFlight),
		userconfig.WindowAnnotationKey:                       api.Spec.Autoscaling.Window.String(),
		userconfig.DownscaleStabilizationPeriodAnnotationKey: api.Spec.Autoscaling.DownscaleStabilizationPeriod.String(),
		userconfig.UpscaleStabilizationPeriodAnnotationKey:   api.Spec.Autoscaling.UpscaleStabilizationPeriod.String(),
		userconfig.MaxDownscaleFactorAnnotationKey:           strings.Float64(api.Spec.Autoscaling.MaxDownscaleFactor.AsApproximateFloat64()),
		userconfig.MaxUpscaleFactorAnnotationKey:             strings.Float64(api.Spec.Autoscaling.MaxUpscaleFactor.AsApproximateFloat64()),
		userconfig.DownscaleToleranceAnnotationKey:           strings.Float64(api.Spec.Autoscaling.DownscaleTolerance.AsApproximateFloat64()),
		userconfig.UpscaleToleranceAnnotationKey:             strings.Float64(api.Spec.Autoscaling.UpscaleTolerance.AsApproximateFloat64()),
	}
}
