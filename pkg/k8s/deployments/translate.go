package deployments

import (
	"encoding/json"
	"fmt"
	"os"

	okLabels "github.com/okteto/okteto/pkg/k8s/labels"
	"github.com/okteto/okteto/pkg/k8s/namespaces"
	"github.com/okteto/okteto/pkg/model"
	"github.com/okteto/okteto/pkg/okteto"

	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	oktetoDeploymentAnnotation = "dev.okteto.com/deployment"
	oktetoDeveloperAnnotation  = "dev.okteto.com/developer"
	oktetoVersionAnnotation    = "dev.okteto.com/version"
	revisionAnnotation         = "deployment.kubernetes.io/revision"
	oktetoBinName              = "okteto-bin"
	oktetoVolumeName           = "okteto"

	//syncthing
	oktetoBinImageTag        = "okteto/bin:1.0.8"
	oktetoSyncSecretVolume   = "okteto-sync-secret"
	oktetoSyncSecretTemplate = "okteto-%s"
)

var (
	devReplicas                      int32 = 1
	devTerminationGracePeriodSeconds int64
)

func translate(t *model.Translation, ns *apiv1.Namespace, c *kubernetes.Clientset) error {
	for _, rule := range t.Rules {
		devContainer := GetDevContainer(&t.Deployment.Spec.Template.Spec, rule.Container)
		if devContainer == nil {
			return fmt.Errorf("Container '%s' not found in deployment '%s'", rule.Container, t.Deployment.Name)
		}
		rule.Container = devContainer.Name
	}

	manifest := getAnnotation(t.Deployment.GetObjectMeta(), oktetoDeploymentAnnotation)
	if manifest != "" {
		dOrig := &appsv1.Deployment{}
		if err := json.Unmarshal([]byte(manifest), dOrig); err != nil {
			return err
		}
		t.Deployment = dOrig
	}
	annotations := t.Deployment.GetObjectMeta().GetAnnotations()
	delete(annotations, revisionAnnotation)
	t.Deployment.GetObjectMeta().SetAnnotations(annotations)

	if c != nil && namespaces.IsOktetoNamespace(ns) {
		_, v := os.LookupEnv("OKTETO_CLIENTSIDE_TRANSLATION")
		if !v {
			commonTranslation(t)
			return setTranslationAsAnnotation(t.Deployment.Spec.Template.GetObjectMeta(), t)
		}
	}

	t.Deployment.Status = appsv1.DeploymentStatus{}
	manifestBytes, err := json.Marshal(t.Deployment)
	if err != nil {
		return err
	}
	setAnnotation(t.Deployment.GetObjectMeta(), oktetoDeploymentAnnotation, string(manifestBytes))

	commonTranslation(t)

	setLabel(t.Deployment.Spec.Template.GetObjectMeta(), okLabels.DevLabel, "true")
	t.Deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = &devTerminationGracePeriodSeconds

	if t.Interactive {
		TranslateOktetoSyncSecret(&t.Deployment.Spec.Template.Spec, t.Name)
	}
	TranslatePodAffinity(&t.Deployment.Spec.Template.Spec, t.Name)
	for _, rule := range t.Rules {
		devContainer := GetDevContainer(&t.Deployment.Spec.Template.Spec, rule.Container)
		TranslateDevContainer(devContainer, rule)
		TranslateOktetoVolumes(&t.Deployment.Spec.Template.Spec, rule)
		TranslatePodSecurityContext(&t.Deployment.Spec.Template.Spec, rule.SecurityContext)
		if rule.Marker != "" {
			TranslateOktetoBinVolumeMounts(devContainer)
			TranslateOktetoInitBinContainer(&t.Deployment.Spec.Template.Spec)
			TranslateOktetoBinVolume(&t.Deployment.Spec.Template.Spec)
		}
	}
	return nil
}

func commonTranslation(t *model.Translation) {
	TranslatePodUserAnnotations(t.Deployment.Spec.Template.GetObjectMeta(), t.Annotations)
	setAnnotation(t.Deployment.GetObjectMeta(), oktetoDeveloperAnnotation, okteto.GetUserID())
	setAnnotation(t.Deployment.GetObjectMeta(), oktetoVersionAnnotation, okLabels.Version)
	setLabel(t.Deployment.GetObjectMeta(), okLabels.DevLabel, "true")

	if t.Interactive {
		setLabel(t.Deployment.Spec.Template.GetObjectMeta(), okLabels.InteractiveDevLabel, t.Name)
	} else {
		setLabel(t.Deployment.Spec.Template.GetObjectMeta(), okLabels.DetachedDevLabel, t.Name)
	}

	t.Deployment.Spec.Replicas = &devReplicas
}

//GetDevContainer returns the dev container of a given deployment
func GetDevContainer(spec *apiv1.PodSpec, name string) *apiv1.Container {
	if len(name) == 0 {
		return &spec.Containers[0]
	}

	for i, c := range spec.Containers {
		if c.Name == name {
			return &spec.Containers[i]
		}
	}

	return nil
}

//TranslatePodUserAnnotations translates the user provided annotations of pod
func TranslatePodUserAnnotations(o metav1.Object, annotations map[string]string) {
	for key, value := range annotations {
		setAnnotation(o, key, value)
	}
}

//TranslatePodAffinity translates the affinity of pod to be all on the same node
func TranslatePodAffinity(spec *apiv1.PodSpec, name string) {
	if spec.Affinity == nil {
		spec.Affinity = &apiv1.Affinity{}
	}
	if spec.Affinity.PodAffinity == nil {
		spec.Affinity.PodAffinity = &apiv1.PodAffinity{}
	}
	if spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = []apiv1.PodAffinityTerm{}
	}
	spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
		spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		apiv1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					okLabels.DevLabel: "true",
				},
			},
			TopologyKey: "kubernetes.io/hostname",
		},
	)
}

//TranslateDevContainer translates a dev container
func TranslateDevContainer(c *apiv1.Container, rule *model.TranslationRule) {
	if len(rule.Image) == 0 {
		rule.Image = c.Image
	}
	c.Image = rule.Image
	c.ImagePullPolicy = rule.ImagePullPolicy

	if rule.WorkDir != "" {
		c.WorkingDir = rule.WorkDir
	}

	if len(rule.Command) > 0 {
		c.Command = rule.Command
		c.Args = rule.Args
	}

	if !rule.Healthchecks {
		c.ReadinessProbe = nil
		c.LivenessProbe = nil
	}

	TranslateResources(c, rule.Resources)
	TranslateEnvVars(c, rule)
	TranslateVolumeMounts(c, rule)
	TranslateContainerSecurityContext(c, rule.SecurityContext)
}

//TranslateResources translates the resources attached to a container
func TranslateResources(c *apiv1.Container, r model.ResourceRequirements) {
	if c.Resources.Requests == nil {
		c.Resources.Requests = make(map[apiv1.ResourceName]resource.Quantity)
	}

	if v, ok := r.Requests[apiv1.ResourceMemory]; ok {
		c.Resources.Requests[apiv1.ResourceMemory] = v
	}

	if v, ok := r.Requests[apiv1.ResourceCPU]; ok {
		c.Resources.Requests[apiv1.ResourceCPU] = v
	}

	if v, ok := r.Requests[model.ResourceAMDGPU]; ok {
		c.Resources.Requests[model.ResourceAMDGPU] = v
	}

	if v, ok := r.Requests[model.ResourceNVIDIAGPU]; ok {
		c.Resources.Requests[model.ResourceNVIDIAGPU] = v
	}

	if c.Resources.Limits == nil {
		c.Resources.Limits = make(map[apiv1.ResourceName]resource.Quantity)
	}

	if v, ok := r.Limits[apiv1.ResourceMemory]; ok {
		c.Resources.Limits[apiv1.ResourceMemory] = v
	}

	if v, ok := r.Limits[apiv1.ResourceCPU]; ok {
		c.Resources.Limits[apiv1.ResourceCPU] = v
	}

	if v, ok := r.Limits[model.ResourceAMDGPU]; ok {
		c.Resources.Limits[model.ResourceAMDGPU] = v
	}

	if v, ok := r.Limits[model.ResourceNVIDIAGPU]; ok {
		c.Resources.Limits[model.ResourceNVIDIAGPU] = v
	}
}

//TranslateEnvVars translates the variables attached to a container
func TranslateEnvVars(c *apiv1.Container, rule *model.TranslationRule) {
	unusedDevEnv := map[string]string{}
	for _, val := range rule.Environment {
		unusedDevEnv[val.Name] = val.Value
	}
	for i, envvar := range c.Env {
		if value, ok := unusedDevEnv[envvar.Name]; ok {
			c.Env[i] = apiv1.EnvVar{Name: envvar.Name, Value: value}
			delete(unusedDevEnv, envvar.Name)
		}
	}
	for _, envvar := range rule.Environment {
		if value, ok := unusedDevEnv[envvar.Name]; ok {
			c.Env = append(c.Env, apiv1.EnvVar{Name: envvar.Name, Value: value})
		}
	}
}

//TranslateVolumeMounts translates the volumes attached to a container
func TranslateVolumeMounts(c *apiv1.Container, rule *model.TranslationRule) {
	if c.VolumeMounts == nil {
		c.VolumeMounts = []apiv1.VolumeMount{}
	}

	for _, v := range rule.Volumes {
		c.VolumeMounts = append(
			c.VolumeMounts,
			apiv1.VolumeMount{
				Name:      v.Name,
				MountPath: v.MountPath,
				SubPath:   v.SubPath,
			},
		)
	}

	if rule.Marker == "" {
		return
	}
	c.VolumeMounts = append(
		c.VolumeMounts,
		apiv1.VolumeMount{
			Name:      oktetoSyncSecretVolume,
			MountPath: "/var/syncthing/secret/",
		},
	)
}

//TranslateOktetoBinVolumeMounts translates the binaries mount attached to a container
func TranslateOktetoBinVolumeMounts(c *apiv1.Container) {
	if c.VolumeMounts == nil {
		c.VolumeMounts = []apiv1.VolumeMount{}
	}
	for _, vm := range c.VolumeMounts {
		if vm.Name == oktetoBinName {
			return
		}
	}
	vm := apiv1.VolumeMount{
		Name:      oktetoBinName,
		MountPath: "/var/okteto/bin",
	}
	c.VolumeMounts = append(c.VolumeMounts, vm)
}

//TranslateOktetoVolumes translates the dev volumes
func TranslateOktetoVolumes(spec *apiv1.PodSpec, rule *model.TranslationRule) {
	if spec.Volumes == nil {
		spec.Volumes = []apiv1.Volume{}
	}
	for _, rV := range rule.Volumes {
		found := false
		for _, v := range spec.Volumes {
			if v.Name == rV.Name {
				found = true
				break
			}
		}
		if found {
			continue
		}
		v := apiv1.Volume{
			Name: rV.Name,
			VolumeSource: apiv1.VolumeSource{
				PersistentVolumeClaim: &apiv1.PersistentVolumeClaimVolumeSource{
					ClaimName: rV.Name,
					ReadOnly:  false,
				},
			},
		}
		spec.Volumes = append(spec.Volumes, v)
	}
}

//TranslateOktetoBinVolume translates the binaries volume attached to a container
func TranslateOktetoBinVolume(spec *apiv1.PodSpec) {
	if spec.Volumes == nil {
		spec.Volumes = []apiv1.Volume{}
	}
	for _, v := range spec.Volumes {
		if v.Name == oktetoBinName {
			return
		}
	}
	v := apiv1.Volume{
		Name: oktetoBinName,
		VolumeSource: apiv1.VolumeSource{
			EmptyDir: &apiv1.EmptyDirVolumeSource{},
		},
	}
	spec.Volumes = append(spec.Volumes, v)
}

//TranslatePodSecurityContext translates the security context attached to a pod
func TranslatePodSecurityContext(spec *apiv1.PodSpec, s *model.SecurityContext) {
	if s == nil {
		return
	}

	if spec.SecurityContext == nil {
		spec.SecurityContext = &apiv1.PodSecurityContext{}
	}

	if s.RunAsUser != nil {
		spec.SecurityContext.RunAsUser = s.RunAsUser
	}

	if s.RunAsGroup != nil {
		spec.SecurityContext.RunAsGroup = s.RunAsGroup
	}

	if s.FSGroup != nil {
		spec.SecurityContext.FSGroup = s.FSGroup
	}
}

//TranslateContainerSecurityContext translates the security context attached to a container
func TranslateContainerSecurityContext(c *apiv1.Container, s *model.SecurityContext) {
	if s == nil || s.Capabilities == nil {
		return
	}

	if c.SecurityContext == nil {
		c.SecurityContext = &apiv1.SecurityContext{}
	}

	if c.SecurityContext.Capabilities == nil {
		c.SecurityContext.Capabilities = &apiv1.Capabilities{}
	}

	c.SecurityContext.ReadOnlyRootFilesystem = nil
	c.SecurityContext.Capabilities.Add = append(c.SecurityContext.Capabilities.Add, s.Capabilities.Add...)
	c.SecurityContext.Capabilities.Drop = append(c.SecurityContext.Capabilities.Drop, s.Capabilities.Drop...)
}

//TranslateOktetoInitBinContainer translates the bin init container of a pod
func TranslateOktetoInitBinContainer(spec *apiv1.PodSpec) {
	c := apiv1.Container{
		Name:            oktetoBinName,
		Image:           oktetoBinImageTag,
		ImagePullPolicy: apiv1.PullIfNotPresent,
		Command:         []string{"sh", "-c", "cp /usr/local/bin/* /okteto/bin"},
		VolumeMounts: []apiv1.VolumeMount{
			{
				Name:      oktetoBinName,
				MountPath: "/okteto/bin",
			},
		},
	}

	if spec.InitContainers == nil {
		spec.InitContainers = []apiv1.Container{}
	}
	spec.InitContainers = append(spec.InitContainers, c)
}

//TranslateOktetoSyncSecret translates the syncthing secret container of a pod
func TranslateOktetoSyncSecret(spec *apiv1.PodSpec, name string) {
	if spec.Volumes == nil {
		spec.Volumes = []apiv1.Volume{}
	}
	for _, s := range spec.Volumes {
		if s.Name == oktetoSyncSecretVolume {
			return
		}
	}
	v := apiv1.Volume{
		Name: oktetoSyncSecretVolume,
		VolumeSource: apiv1.VolumeSource{
			Secret: &apiv1.SecretVolumeSource{
				SecretName: fmt.Sprintf(oktetoSyncSecretTemplate, name),
			},
		},
	}
	spec.Volumes = append(spec.Volumes, v)
}
