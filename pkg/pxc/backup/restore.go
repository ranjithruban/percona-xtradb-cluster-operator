package backup

import (
	"strings"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/percona/percona-xtradb-cluster-operator/pkg/apis/pxc/v1alpha1"
	"github.com/percona/percona-xtradb-cluster-operator/pkg/pxc/app"
)

func PVCRestoreService(cr *api.PerconaXtraDBBackupRestore, bcp *api.PerconaXtraDBBackup) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-src-" + cr.Name + "-" + bcp.Spec.PXCCluster,
			Namespace: bcp.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"name": "restore-src-" + cr.Name + "-" + bcp.Spec.PXCCluster,
			},
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Port: 3307,
					Name: "ncat",
				},
			},
		},
	}
}

func PVCRestorePod(cr *api.PerconaXtraDBBackupRestore, bcp *api.PerconaXtraDBBackup, pvcName string, cluster api.PerconaXtraDBClusterSpec) *corev1.Pod {
	podPVC := corev1.Volume{
		Name: "backup",
	}
	podPVC.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: pvcName,
	}

	podPVCs := []corev1.Volume{
		podPVC,
		app.GetSecretVolumes("ssl-internal", cluster.PXC.SSLInternalSecretName, true),
		app.GetSecretVolumes("ssl", cluster.PXC.SSLSecretName, cluster.PXC.AllowUnsafeConfig),
	}
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-src-" + cr.Name + "-" + bcp.Spec.PXCCluster,
			Namespace: bcp.Namespace,
			Labels: map[string]string{
				"name": "restore-src-" + cr.Name + "-" + bcp.Spec.PXCCluster,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "ncat",
					Image:           cluster.Backup.Image,
					ImagePullPolicy: corev1.PullAlways,
					Command: []string{
						"bash",
						"-exc",
						"cat /backup/xtrabackup.stream | ncat -l --send-only 3307",
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "backup",
							MountPath: "/backup",
						},
						{
							Name:      "ssl",
							MountPath: "/etc/mysql/ssl",
						},
						{
							Name:      "ssl-internal",
							MountPath: "/etc/mysql/ssl-internal",
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyAlways,
			Volumes:       podPVCs,
		},
	}
}

func PVCRestoreJob(cr *api.PerconaXtraDBBackupRestore, bcp *api.PerconaXtraDBBackup, cluster api.PerconaXtraDBClusterSpec) *batchv1.Job {
	jobPVC := corev1.Volume{
		Name: "datadir",
	}
	jobPVC.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: "datadir-" + bcp.Spec.PXCCluster + "-pxc-0",
	}

	jobPVCs := []corev1.Volume{
		jobPVC,
		app.GetSecretVolumes("ssl-internal", cluster.PXC.SSLInternalSecretName, true),
		app.GetSecretVolumes("ssl", cluster.PXC.SSLSecretName, cluster.PXC.AllowUnsafeConfig),
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-job-" + cr.Name + "-" + bcp.Spec.PXCCluster,
			Namespace: bcp.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "xtrabackup",
							Image:           cluster.Backup.Image,
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"bash",
								"-exc",
								`ping -c1 restore-src-` + cr.Name + "-" + bcp.Spec.PXCCluster + ` || :
								 rm -rf /datadir/*
								 ncat restore-src-` + cr.Name + "-" + bcp.Spec.PXCCluster + ` 3307 | xbstream -x -C /datadir
								 xtrabackup --prepare --target-dir=/datadir`,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "datadir",
									MountPath: "/datadir",
								},
								{
									Name:      "ssl",
									MountPath: "/etc/mysql/ssl",
								},
								{
									Name:      "ssl-internal",
									MountPath: "/etc/mysql/ssl-internal",
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
					Volumes:       jobPVCs,
				},
			},
			BackoffLimit: func(i int32) *int32 { return &i }(4),
		},
	}

	useMem, k8sq, err := xbMemoryUse(cluster)

	if useMem != "" && err == nil {
		job.Spec.Template.Spec.Containers[0].Env = append(
			job.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{
				Name:  "XB_USE_MEMORY",
				Value: useMem,
			},
		)
		job.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceMemory: k8sq,
		}
	}

	return job
}

// S3RestoreJob returns restore job object for s3
func S3RestoreJob(cr *api.PerconaXtraDBBackupRestore, bcp *api.PerconaXtraDBBackup, s3dest string, cluster api.PerconaXtraDBClusterSpec) (*batchv1.Job, error) {
	if bcp.Status.S3 == nil {
		return nil, errors.New("nil s3 backup status")
	}

	jobPVC := corev1.Volume{
		Name: "datadir",
	}
	jobPVC.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: "datadir-" + bcp.Spec.PXCCluster + "-pxc-0",
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-job-" + cr.Name + "-" + bcp.Spec.PXCCluster,
			Namespace: bcp.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "xtrabackup",
							Image:           cluster.Backup.Image,
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"bash",
								"-exc",
								`mc -C /tmp/mc config host add dest "${AWS_ENDPOINT_URL:-https://s3.amazonaws.com}" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY"
								 mc -C /tmp/mc ls dest/` + s3dest + `
								 rm -rf /datadir/*
								 mc -C /tmp/mc cat dest/` + s3dest + ` | xbstream -x -C /datadir
								 xtrabackup --prepare --target-dir=/datadir`,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "datadir",
									MountPath: "/datadir",
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "AWS_ENDPOINT_URL",
									Value: bcp.Status.S3.EndpointURL,
								},
								{
									Name: "AWS_ACCESS_KEY_ID",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: bcp.Status.S3.CredentialsSecret,
											},
											Key: "AWS_ACCESS_KEY_ID",
										},
									},
								},
								{
									Name: "AWS_SECRET_ACCESS_KEY",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: bcp.Status.S3.CredentialsSecret,
											},
											Key: "AWS_SECRET_ACCESS_KEY",
										},
									},
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{
						jobPVC,
					},
				},
			},
			BackoffLimit: func(i int32) *int32 { return &i }(4),
		},
	}

	useMem, k8sq, err := xbMemoryUse(cluster)

	if useMem != "" && err == nil {
		job.Spec.Template.Spec.Containers[0].Env = append(
			job.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{
				Name:  "XB_USE_MEMORY",
				Value: useMem,
			},
		)
		job.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceMemory: k8sq,
		}
	}

	return job, nil
}

func xbMemoryUse(cluster api.PerconaXtraDBClusterSpec) (useMem string, k8sQuantity resource.Quantity, err error) {
	if cluster.PXC.Resources != nil {
		if cluster.PXC.Resources.Requests != nil {
			useMem = cluster.PXC.Resources.Requests.Memory
			k8sQuantity, err = resource.ParseQuantity(cluster.PXC.Resources.Requests.Memory)
		}

		if cluster.PXC.Resources.Limits != nil && cluster.PXC.Resources.Limits.Memory != "" {
			useMem = cluster.PXC.Resources.Limits.Memory
			k8sQuantity, err = resource.ParseQuantity(cluster.PXC.Resources.Limits.Memory)
		}

		// make the 90% value
		q := k8sQuantity.DeepCopy()
		q.Sub(*resource.NewQuantity(k8sQuantity.Value()/10, k8sQuantity.Format))
		useMem = q.String()
		// transform Gi/Mi/etc to G/M
		if strings.Contains(useMem, "i") {
			useMem = strings.Replace(useMem, "i", "", -1)
		}
	}

	return useMem, k8sQuantity, err
}
