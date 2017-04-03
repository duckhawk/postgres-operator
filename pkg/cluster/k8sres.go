package cluster

import (
	"fmt"

	"k8s.io/client-go/pkg/api/resource"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/apps/v1beta1"
	"k8s.io/client-go/pkg/util/intstr"

	"github.bus.zalan.do/acid/postgres-operator/pkg/spec"
	"github.bus.zalan.do/acid/postgres-operator/pkg/util"
	"github.bus.zalan.do/acid/postgres-operator/pkg/util/constants"
)

func resourceList(resources spec.Resources) *v1.ResourceList {
	resourceList := v1.ResourceList{}
	if resources.Cpu != "" {
		resourceList[v1.ResourceCPU] = resource.MustParse(resources.Cpu)
	}

	if resources.Memory != "" {
		resourceList[v1.ResourceMemory] = resource.MustParse(resources.Memory)
	}

	return &resourceList
}

func (c *Cluster) genPodTemplate(resourceList *v1.ResourceList, pgVersion string) *v1.PodTemplateSpec {
	envVars := []v1.EnvVar{
		{
			Name:  "SCOPE",
			Value: c.Metadata.Name,
		},
		{
			Name:  "PGROOT",
			Value: "/home/postgres/pgdata/pgroot",
		},
		{
			Name:  "ETCD_HOST",
			Value: c.OpConfig.EtcdHost,
		},
		{
			Name: "POD_IP",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.podIP",
				},
			},
		},
		{
			Name: "POD_NAMESPACE",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.namespace",
				},
			},
		},
		{
			Name: "PGPASSWORD_SUPERUSER",
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: c.credentialSecretName(c.OpConfig.SuperUsername),
					},
					Key: "password",
				},
			},
		},
		{
			Name: "PGPASSWORD_STANDBY",
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: c.credentialSecretName(c.OpConfig.ReplicationUsername),
					},
					Key: "password",
				},
			},
		},
		{
			Name:  "PAM_OAUTH2",
			Value: c.OpConfig.PamConfiguration,
		},
		{
			Name: "SPILO_CONFIGURATION",
			Value: fmt.Sprintf(`
postgresql:
  bin_dir: /usr/lib/postgresql/%s/bin
bootstrap:
  initdb:
  - auth-host: md5
  - auth-local: trust
  users:
    %s:
      password: NULL
      options:
        - createdb
        - nologin
  pg_hba:
  - hostnossl all all all reject
  - hostssl   all +%s all pam
  - hostssl   all all all md5`, pgVersion, c.OpConfig.PamRoleName, c.OpConfig.PamRoleName),
		},
	}

	container := v1.Container{
		Name:            c.Metadata.Name,
		Image:           c.OpConfig.DockerImage,
		ImagePullPolicy: v1.PullAlways,
		Resources: v1.ResourceRequirements{
			Requests: *resourceList,
		},
		Ports: []v1.ContainerPort{
			{
				ContainerPort: 8008,
				Protocol:      v1.ProtocolTCP,
			},
			{
				ContainerPort: 5432,
				Protocol:      v1.ProtocolTCP,
			},
			{
				ContainerPort: 8080,
				Protocol:      v1.ProtocolTCP,
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      constants.DataVolumeName,
				MountPath: "/home/postgres/pgdata", //TODO: fetch from manifesto
			},
		},
		Env: envVars,
	}
	terminateGracePeriodSeconds := int64(30)

	podSpec := v1.PodSpec{
		ServiceAccountName:            c.OpConfig.ServiceAccountName,
		TerminationGracePeriodSeconds: &terminateGracePeriodSeconds,
		Containers:                    []v1.Container{container},
	}

	template := v1.PodTemplateSpec{
		ObjectMeta: v1.ObjectMeta{
			Labels:    c.labelsSet(),
			Namespace: c.Metadata.Name,
		},
		Spec: podSpec,
	}

	return &template
}

func (c *Cluster) genStatefulSet(spec spec.PostgresSpec) *v1beta1.StatefulSet {
	resourceList := resourceList(spec.Resources)
	podTemplate := c.genPodTemplate(resourceList, spec.PgVersion)
	volumeClaimTemplate := persistentVolumeClaimTemplate(spec.Volume.Size, spec.Volume.StorageClass)

	statefulSet := &v1beta1.StatefulSet{
		ObjectMeta: v1.ObjectMeta{
			Name:      c.Metadata.Name,
			Namespace: c.Metadata.Namespace,
			Labels:    c.labelsSet(),
		},
		Spec: v1beta1.StatefulSetSpec{
			Replicas:             &spec.NumberOfInstances,
			ServiceName:          c.Metadata.Name,
			Template:             *podTemplate,
			VolumeClaimTemplates: []v1.PersistentVolumeClaim{*volumeClaimTemplate},
		},
	}

	return statefulSet
}

func persistentVolumeClaimTemplate(volumeSize, volumeStorageClass string) *v1.PersistentVolumeClaim {
	metadata := v1.ObjectMeta{
		Name: constants.DataVolumeName,
	}
	if volumeStorageClass != "" {
		// TODO: check if storage class exists
		metadata.Annotations = map[string]string{"volume.beta.kubernetes.io/storage-class": volumeStorageClass}
	} else {
		metadata.Annotations = map[string]string{"volume.alpha.kubernetes.io/storage-class": "default"}
	}

	volumeClaim := &v1.PersistentVolumeClaim{
		ObjectMeta: metadata,
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: resource.MustParse(volumeSize),
				},
			},
		},
	}
	return volumeClaim
}

func (c *Cluster) genUserSecrets() (secrets map[string]*v1.Secret, err error) {
	secrets = make(map[string]*v1.Secret, len(c.pgUsers))
	namespace := c.Metadata.Namespace
	for username, pgUser := range c.pgUsers {
		//Skip users with no password i.e. human users (they'll be authenticated using pam)
		if pgUser.Password == "" {
			continue
		}
		secret := v1.Secret{
			ObjectMeta: v1.ObjectMeta{
				Name:      c.credentialSecretName(username),
				Namespace: namespace,
				Labels:    c.labelsSet(),
			},
			Type: v1.SecretTypeOpaque,
			Data: map[string][]byte{
				"username": []byte(pgUser.Name),
				"password": []byte(pgUser.Password),
			},
		}
		secrets[username] = &secret
	}

	return
}

func (c *Cluster) genService(allowedSourceRanges []string) *v1.Service {
	service := &v1.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:      c.Metadata.Name,
			Namespace: c.Metadata.Namespace,
			Labels:    c.labelsSet(),
			Annotations: map[string]string{
				constants.ZalandoDnsNameAnnotation: util.ClusterDNSName(c.Metadata.Name, c.TeamName(), c.OpConfig.DbHostedZone),
			},
		},
		Spec: v1.ServiceSpec{
			Type:  v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{{Port: 5432, TargetPort: intstr.IntOrString{IntVal: 5432}}},
			LoadBalancerSourceRanges: allowedSourceRanges,
		},
	}

	return service
}

func (c *Cluster) genEndpoints() *v1.Endpoints {
	endpoints := &v1.Endpoints{
		ObjectMeta: v1.ObjectMeta{
			Name:      c.Metadata.Name,
			Namespace: c.Metadata.Namespace,
			Labels:    c.labelsSet(),
		},
	}

	return endpoints
}