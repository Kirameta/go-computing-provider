package computing

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lagrangedao/go-computing-provider/docker"
	"github.com/lagrangedao/go-computing-provider/yaml"
	"io"
	appV1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lagrangedao/go-computing-provider/conf"

	"github.com/filswan/go-mcs-sdk/mcs/api/common/logs"
	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/lagrangedao/go-computing-provider/common"
	"github.com/lagrangedao/go-computing-provider/constants"
	"github.com/lagrangedao/go-computing-provider/models"
	coreV1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func GetServiceProviderInfo(c *gin.Context) {
	info := new(models.HostInfo)
	//info.SwanProviderVersion = common.GetVersion()
	info.OperatingSystem = runtime.GOOS
	info.Architecture = runtime.GOARCH
	info.CPUCores = runtime.NumCPU()
	c.JSON(http.StatusOK, common.CreateSuccessResponse(info))
}

func ReceiveJob(c *gin.Context) {
	var jobData models.JobData
	if err := c.ShouldBindJSON(&jobData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logs.GetLogger().Infof("Job received Data: %+v", jobData)

	var hostName string
	prefixStr := generateString(10)
	if strings.HasPrefix(conf.GetConfig().API.Domain, ".") {
		hostName = prefixStr + conf.GetConfig().API.Domain
	} else {
		hostName = strings.Join([]string{prefixStr, conf.GetConfig().API.Domain}, ".")
	}

	delayTask, err := celeryService.DelayTask(constants.TASK_DEPLOY, jobData.JobSourceURI, hostName, jobData.Duration, jobData.UUID)
	if err != nil {
		logs.GetLogger().Errorf("Failed sync delpoy task, error: %v", err)
		return
	}
	go func() {
		result, err := delayTask.Get(180 * time.Second)
		if err != nil {
			logs.GetLogger().Errorf("Failed get sync task result, error: %v", err)
			return
		}
		logs.GetLogger().Infof("Job_uuid: %s, service running successfully, job_result_url: %s", jobData.UUID, result.(string))
	}()

	jobData.JobResultURI = ""
	submitJob(&jobData)
	updateJobStatus(jobData.UUID, models.JobUploadResult)
	c.JSON(http.StatusOK, jobData)
}

func submitJob(jobData *models.JobData) {
	logs.GetLogger().Printf("submitting job...")
	oldMask := syscall.Umask(0)
	defer syscall.Umask(oldMask)

	fileCachePath := conf.GetConfig().MCS.FileCachePath
	folderPath := "jobs"
	jobDetailFile := filepath.Join(folderPath, uuid.NewString()+".json")
	os.MkdirAll(filepath.Join(fileCachePath, folderPath), os.ModePerm)
	taskDetailFilePath := filepath.Join(fileCachePath, jobDetailFile)

	jobData.Status = constants.BiddingSubmitted
	jobData.UpdatedAt = strconv.FormatInt(time.Now().Unix(), 10)
	bytes, err := json.Marshal(jobData)
	if err != nil {
		logs.GetLogger().Errorf("Failed Marshal JobData, error: %v", err)
		return
	}
	if err = os.WriteFile(taskDetailFilePath, bytes, os.ModePerm); err != nil {
		logs.GetLogger().Errorf("Failed jobData write to file, error: %v", err)
		return
	}

	storageService := NewStorageService()
	mcsOssFile, err := storageService.UploadFileToBucket(jobDetailFile, taskDetailFilePath, true)
	if err != nil {
		logs.GetLogger().Errorf("Failed upload file to bucket, error: %v", err)
		return
	}
	mcsFileJson, _ := json.Marshal(mcsOssFile)
	logs.GetLogger().Printf("Job submitted to IPFS %s", string(mcsFileJson))

	gatewayUrl, err := storageService.GetGatewayUrl()
	if err != nil {
		logs.GetLogger().Errorf("Failed get mcs ipfs gatewayUrl, error: %v", err)
		return
	}
	jobData.JobResultURI = *gatewayUrl + "/ipfs/" + mcsOssFile.PayloadCid
}

func RedeployJob(c *gin.Context) {
	var jobData models.JobData

	if err := c.ShouldBindJSON(&jobData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logs.GetLogger().Infof("redeploy Job received: %+v", jobData)

	var hostName string
	if jobData.JobResultURI != "" {
		resp, err := http.Get(jobData.JobResultURI)
		if err != nil {
			logs.GetLogger().Errorf("error making request to Space API: %+v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		defer func(Body io.ReadCloser) {
			err := Body.Close()
			if err != nil {
				logs.GetLogger().Errorf("error closed resp Space API: %+v", err)
			}
		}(resp.Body)
		logs.GetLogger().Infof("Space API response received. Response: %d", resp.StatusCode)
		if resp.StatusCode != http.StatusOK {
			logs.GetLogger().Errorf("space API response not OK. Status Code: %d", resp.StatusCode)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}

		var hostInfo struct {
			JobResultUri string `json:"job_result_uri"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&hostInfo); err != nil {
			logs.GetLogger().Errorf("error decoding Space API response JSON: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hostName = strings.ReplaceAll(hostInfo.JobResultUri, "https://", "")
	} else {
		hostName = generateString(10) + conf.GetConfig().API.Domain
	}

	delayTask, err := celeryService.DelayTask(constants.TASK_DEPLOY, jobData.JobResultURI, hostName, jobData.Duration, jobData.UUID)
	if err != nil {
		logs.GetLogger().Errorf("Failed sync delpoy task, error: %v", err)
		return
	}
	logs.GetLogger().Infof("delayTask detail info: %+v", delayTask)

	go func() {
		result, err := delayTask.Get(180 * time.Second)
		if err != nil {
			logs.GetLogger().Errorf("Failed get sync task result, error: %v", err)
			return
		}
		logs.GetLogger().Infof("Job: %s, service running successfully, job_result_url: %s", jobData.JobResultURI, result.(string))
	}()

	jobData.JobResultURI = fmt.Sprintf("https://%s", hostName)
	submitJob(&jobData)
	c.JSON(http.StatusOK, jobData)
}

func ReNewJob(c *gin.Context) {
	var jobData struct {
		JobUuid  string `json:"job_uuid"`
		Duration int    `json:"duration"`
	}

	if err := c.ShouldBindJSON(&jobData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logs.GetLogger().Infof("renew Job received: %+v", jobData)

	conn := redisPool.Get()
	redisKey := constants.REDIS_FULL_PREFIX + jobData.JobUuid
	args := []interface{}{redisKey}
	args = append(args, "k8s_namespace", "space_name", "expire_time", "space_uuid")
	valuesStr, err := redis.Strings(conn.Do("HMGET", args...))
	if err != nil {
		logs.GetLogger().Errorf("Failed get redis key data, key: %s, error: %+v", redisKey, err)
		c.JSON(http.StatusOK, map[string]string{
			"status":  "failed",
			"message": "The job was terminated due to its expiration date",
		})
	}

	var (
		namespace string
		spaceName string
		leftTime  int64
		spaceUuid string
	)
	if len(valuesStr) >= 4 {
		namespace = valuesStr[0]
		spaceName = valuesStr[1]
		expireTimeStr := valuesStr[2]
		spaceUuid = valuesStr[3]
		expireTime, err := strconv.ParseInt(strings.TrimSpace(expireTimeStr), 10, 64)
		if err != nil {
			logs.GetLogger().Errorf("Failed convert time str: [%s], error: %+v", expireTimeStr, err)
			return
		}
		leftTime = expireTime - time.Now().Unix()
	}

	if leftTime < 0 {
		c.JSON(http.StatusOK, map[string]string{
			"status":  "failed",
			"message": "The job was terminated due to its expiration date",
		})
	} else {
		fullArgs := []interface{}{redisKey}
		fields := map[string]string{
			"k8s_namespace": namespace,
			"space_name":    spaceName,
			"expire_time":   strconv.Itoa(int(time.Now().Unix()) + int(leftTime) + jobData.Duration),
			"space_uuid":    spaceUuid,
		}
		for key, val := range fields {
			fullArgs = append(fullArgs, key, val)
		}
		conn.Do("HSET", fullArgs...)
		conn.Do("SET", jobData.JobUuid, "wait-delete", "EX", int(leftTime)+jobData.Duration)
	}
	c.JSON(http.StatusOK, map[string]string{
		"status": "success",
	})
	c.JSON(http.StatusOK, common.CreateSuccessResponse("success"))
}

func DeleteJob(c *gin.Context) {
	creatorWallet := c.Query("creator_wallet")
	spaceUuid := c.Query("space_uuid")

	if creatorWallet == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "creator_wallet is required"})
	}
	if spaceUuid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "space_uuid is required"})
	}

	k8sNameSpace := constants.K8S_NAMESPACE_NAME_PREFIX + strings.ToLower(creatorWallet)
	deleteJob(k8sNameSpace, spaceUuid)
	c.JSON(http.StatusOK, common.CreateSuccessResponse("deleted success"))
}

func StatisticalSources(c *gin.Context) {
	location, err := getLocation()
	if err != nil {
		logs.GetLogger().Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed get location info"})
	}

	k8sService := NewK8sService()
	statisticalSources, err := k8sService.StatisticalSources(context.TODO())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}

	nodeID, _, _ := generateNodeID()
	c.JSON(http.StatusOK, models.ClusterResource{
		NodeId:      nodeID,
		Region:      location,
		ClusterInfo: statisticalSources,
	})
}

func DeploySpaceTask(jobSourceURI, hostName string, duration int, jobUuid string) string {
	defer func() {
		if err := recover(); err != nil {
			logs.GetLogger().Errorf("deploy space task painc, error: %+v", err)
			return
		}
	}()
	var gpuName string
	defer func() {
		if gpuName != "" {
			count, ok := runTaskGpuResource.Load(gpuName)
			if ok && count.(int) > 0 {
				runTaskGpuResource.Store(gpuName, count.(int)-1)
			} else {
				runTaskGpuResource.Delete(gpuName)
			}
		}
	}()

	resp, err := http.Get(jobSourceURI)
	if err != nil {
		logs.GetLogger().Errorf("error making request to Space API: %+v", err)
		return ""
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logs.GetLogger().Errorf("error closed resp Space API: %+v", err)
		}
	}(resp.Body)
	logs.GetLogger().Infof("Space API response received. Response: %d", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		logs.GetLogger().Errorf("space API response not OK. Status Code: %d", resp.StatusCode)
		return ""
	}

	var spaceJson models.SpaceJSON
	if err := json.NewDecoder(resp.Body).Decode(&spaceJson); err != nil {
		logs.GetLogger().Errorf("error decoding Space API response JSON: %v", err)
		return ""
	}

	creator := strings.ToLower(spaceJson.Data.Owner.PublicAddress)
	spaceName := strings.ToLower(spaceJson.Data.Space.Name)
	spaceUuid := strings.ToLower(spaceJson.Data.Space.Uuid)
	spaceHardware := spaceJson.Data.Space.ActiveOrder.Config

	logs.GetLogger().Infof("uuid: %s, spaceName: %s, hardwareName: %s", spaceUuid, spaceName, spaceHardware.Description)
	if len(spaceHardware.Description) == 0 {
		return ""
	}
	hardwareInfo := getHardwareDetail(spaceHardware.Description)

	if hardwareInfo.Gpu.Unit != "" {
		gpuName = strings.ReplaceAll(hardwareInfo.Gpu.Unit, " ", "-")
		count, ok := runTaskGpuResource.Load(gpuName)
		if ok {
			runTaskGpuResource.Store(gpuName, count.(int)+1)
		} else {
			runTaskGpuResource.Store(gpuName, 1)
		}
	}

	updateJobStatus(jobUuid, models.JobDownloadSource)
	containsYaml, yamlPath, imagePath, err := BuildSpaceTaskImage(spaceUuid, spaceJson.Data.Files)
	if err != nil {
		logs.GetLogger().Error(err)
		return ""
	}

	if containsYaml {
		yamlToK8s(jobUuid, creator, spaceUuid, yamlPath, hostName, hardwareInfo, duration)
	} else {
		imageName, dockerfilePath := BuildImagesByDockerfile(jobUuid, spaceUuid, spaceName, imagePath)
		dockerfileToK8s(jobUuid, hostName, creator, spaceUuid, imageName, dockerfilePath, hardwareInfo, duration)
	}
	return hostName
}

func dockerfileToK8s(jobUuid, hostName, creatorWallet, spaceUuid, imageName, dockerfilePath string, hardwareResource models.Resource, duration int) {
	exposedPort, err := docker.ExtractExposedPort(dockerfilePath)
	if err != nil {
		logs.GetLogger().Infof("Failed to extract exposed port: %v", err)
		return
	}
	containerPort, err := strconv.ParseInt(exposedPort, 10, 64)
	if err != nil {
		logs.GetLogger().Errorf("Failed to convert exposed port: %v", err)
		return
	}

	// first delete old resource
	k8sNameSpace := constants.K8S_NAMESPACE_NAME_PREFIX + creatorWallet
	deleteJob(k8sNameSpace, spaceUuid)

	if err := deployNamespace(creatorWallet); err != nil {
		logs.GetLogger().Error(err)
		return
	}

	memQuantity, err := resource.ParseQuantity(fmt.Sprintf("%d%s", hardwareResource.Memory.Quantity, hardwareResource.Memory.Unit))
	if err != nil {
		logs.GetLogger().Errorf("get memory failed, error: %+v", err)
		return
	}

	storageQuantity, err := resource.ParseQuantity(fmt.Sprintf("%d%s", hardwareResource.Storage.Quantity, hardwareResource.Storage.Unit))
	if err != nil {
		logs.GetLogger().Errorf("get storage failed, error: %+v", err)
		return
	}

	// create deployment
	k8sService := NewK8sService()
	deployment := &appV1.Deployment{
		TypeMeta: metaV1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metaV1.ObjectMeta{
			Name:      constants.K8S_DEPLOY_NAME_PREFIX + spaceUuid,
			Namespace: k8sNameSpace,
		},
		Spec: appV1.DeploymentSpec{
			Selector: &metaV1.LabelSelector{
				MatchLabels: map[string]string{"lad_app": spaceUuid},
			},

			Template: coreV1.PodTemplateSpec{
				ObjectMeta: metaV1.ObjectMeta{
					Labels:    map[string]string{"lad_app": spaceUuid},
					Namespace: k8sNameSpace,
				},

				Spec: coreV1.PodSpec{
					NodeSelector: generateLabel(hardwareResource.Gpu.Unit),
					Containers: []coreV1.Container{{
						Name:            constants.K8S_CONTAINER_NAME_PREFIX + spaceUuid,
						Image:           imageName,
						ImagePullPolicy: coreV1.PullIfNotPresent,
						Ports: []coreV1.ContainerPort{{
							ContainerPort: int32(containerPort),
						}},
						Env: []coreV1.EnvVar{
							{
								Name:  "wallet_address",
								Value: creatorWallet,
							},
							{
								Name:  "space_uuid",
								Value: spaceUuid,
							},
							{
								Name:  "result_url",
								Value: hostName,
							},
							{
								Name:  "job_uuid",
								Value: jobUuid,
							},
						},
						Resources: coreV1.ResourceRequirements{
							Limits: coreV1.ResourceList{
								coreV1.ResourceCPU:              *resource.NewQuantity(hardwareResource.Cpu.Quantity, resource.DecimalSI),
								coreV1.ResourceMemory:           memQuantity,
								coreV1.ResourceEphemeralStorage: storageQuantity,
								"nvidia.com/gpu":                resource.MustParse(fmt.Sprintf("%d", hardwareResource.Gpu.Quantity)),
							},
							Requests: coreV1.ResourceList{
								coreV1.ResourceCPU:              *resource.NewQuantity(hardwareResource.Cpu.Quantity, resource.DecimalSI),
								coreV1.ResourceMemory:           memQuantity,
								coreV1.ResourceEphemeralStorage: storageQuantity,
								"nvidia.com/gpu":                resource.MustParse(fmt.Sprintf("%d", hardwareResource.Gpu.Quantity)),
							},
						},
					}},
				},
			},
		}}
	createDeployment, err := k8sService.CreateDeployment(context.TODO(), k8sNameSpace, deployment)
	if err != nil {
		logs.GetLogger().Error(err)
		return
	}

	updateJobStatus(jobUuid, models.JobPullImage)
	logs.GetLogger().Infof("Created deployment: %s", createDeployment.GetObjectMeta().GetName())

	if err := deployK8sResource(k8sNameSpace, spaceUuid, hostName, containerPort); err != nil {
		logs.GetLogger().Error(err)
		return
	}
	updateJobStatus(jobUuid, models.JobDeployToK8s)

	watchContainerRunningTime(jobUuid, k8sNameSpace, spaceUuid, int64(duration))
	return
}

func yamlToK8s(jobUuid, creatorWallet, spaceUuid, yamlPath, hostName string, hardwareResource models.Resource, duration int) {
	k8sNameSpace := constants.K8S_NAMESPACE_NAME_PREFIX + creatorWallet
	deleteJob(k8sNameSpace, spaceUuid)

	containerResources, err := yaml.HandlerYaml(yamlPath)
	if err != nil {
		logs.GetLogger().Error(err)
		return
	}

	if err := deployNamespace(creatorWallet); err != nil {
		logs.GetLogger().Error(err)
		return
	}

	memQuantity, err := resource.ParseQuantity(fmt.Sprintf("%d%s", hardwareResource.Memory.Quantity, hardwareResource.Memory.Unit))
	if err != nil {
		logs.GetLogger().Error("get memory failed, error: %+v", err)
		return
	}

	storageQuantity, err := resource.ParseQuantity(fmt.Sprintf("%d%s", hardwareResource.Storage.Quantity, hardwareResource.Storage.Unit))
	if err != nil {
		logs.GetLogger().Error("get storage failed, error: %+v", err)
		return
	}

	k8sService := NewK8sService()
	for _, cr := range containerResources {
		for i, envVar := range cr.Env {
			if strings.Contains(envVar.Name, "NEXTAUTH_URL") {
				cr.Env[i].Value = "https://" + hostName
				break
			}
		}

		var volumeMount []coreV1.VolumeMount
		var volumes []coreV1.Volume
		if cr.VolumeMounts.Path != "" {
			fileNameWithoutExt := filepath.Base(cr.VolumeMounts.Name[:len(cr.VolumeMounts.Name)-len(filepath.Ext(cr.VolumeMounts.Name))])
			configMap, err := k8sService.CreateConfigMap(context.TODO(), k8sNameSpace, spaceUuid, filepath.Dir(yamlPath), cr.VolumeMounts.Name)
			if err != nil {
				logs.GetLogger().Error(err)
				return
			}
			configName := configMap.GetName()
			volumes = []coreV1.Volume{
				{
					Name: spaceUuid + "-" + fileNameWithoutExt,
					VolumeSource: coreV1.VolumeSource{
						ConfigMap: &coreV1.ConfigMapVolumeSource{
							LocalObjectReference: coreV1.LocalObjectReference{
								Name: configName,
							},
						},
					},
				},
			}
			volumeMount = []coreV1.VolumeMount{
				{
					Name:      spaceUuid + "-" + fileNameWithoutExt,
					MountPath: cr.VolumeMounts.Path,
				},
			}
		}

		var containers []coreV1.Container
		for _, depend := range cr.Depends {
			var handler = new(coreV1.ExecAction)
			handler.Command = depend.ReadyCmd
			containers = append(containers, coreV1.Container{
				Name:            spaceUuid + "-" + depend.Name,
				Image:           depend.ImageName,
				Command:         depend.Command,
				Args:            depend.Args,
				Env:             depend.Env,
				Ports:           depend.Ports,
				ImagePullPolicy: coreV1.PullIfNotPresent,
				Resources:       coreV1.ResourceRequirements{},
				ReadinessProbe: &coreV1.Probe{
					ProbeHandler: coreV1.ProbeHandler{
						Exec: handler,
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
				},
			})
		}

		cr.Env = append(cr.Env, []coreV1.EnvVar{
			{
				Name:  "wallet_address",
				Value: creatorWallet,
			},
			{
				Name:  "space_uuid",
				Value: spaceUuid,
			},
			{
				Name:  "result_url",
				Value: hostName,
			},
			{
				Name:  "job_uuid",
				Value: jobUuid,
			},
		}...)

		containers = append(containers, coreV1.Container{
			Name:            spaceUuid + "-" + cr.Name,
			Image:           cr.ImageName,
			Command:         cr.Command,
			Args:            cr.Args,
			Env:             cr.Env,
			Ports:           cr.Ports,
			ImagePullPolicy: coreV1.PullIfNotPresent,
			Resources: coreV1.ResourceRequirements{
				Limits: coreV1.ResourceList{
					coreV1.ResourceCPU:              *resource.NewQuantity(hardwareResource.Cpu.Quantity, resource.DecimalSI),
					coreV1.ResourceMemory:           memQuantity,
					coreV1.ResourceEphemeralStorage: storageQuantity,
					"nvidia.com/gpu":                resource.MustParse(fmt.Sprintf("%d", hardwareResource.Gpu.Quantity)),
				},
				Requests: coreV1.ResourceList{
					coreV1.ResourceCPU:              *resource.NewQuantity(hardwareResource.Cpu.Quantity, resource.DecimalSI),
					coreV1.ResourceMemory:           memQuantity,
					coreV1.ResourceEphemeralStorage: storageQuantity,
					"nvidia.com/gpu":                resource.MustParse(fmt.Sprintf("%d", hardwareResource.Gpu.Quantity)),
				},
			},
			VolumeMounts: volumeMount,
		})

		deployment := &appV1.Deployment{
			TypeMeta: metaV1.TypeMeta{
				Kind:       "Deployment",
				APIVersion: "apps/v1",
			},
			ObjectMeta: metaV1.ObjectMeta{
				Name:      constants.K8S_DEPLOY_NAME_PREFIX + spaceUuid,
				Namespace: k8sNameSpace,
			},

			Spec: appV1.DeploymentSpec{
				Selector: &metaV1.LabelSelector{
					MatchLabels: map[string]string{"lad_app": spaceUuid},
				},
				Template: coreV1.PodTemplateSpec{
					ObjectMeta: metaV1.ObjectMeta{
						Labels:    map[string]string{"lad_app": spaceUuid},
						Namespace: k8sNameSpace,
					},
					Spec: coreV1.PodSpec{
						NodeSelector: generateLabel(hardwareResource.Gpu.Unit),
						Containers:   containers,
						Volumes:      volumes,
					},
				},
			}}

		createDeployment, err := k8sService.CreateDeployment(context.TODO(), k8sNameSpace, deployment)
		if err != nil {
			logs.GetLogger().Error(err)
			return
		}

		updateJobStatus(jobUuid, models.JobPullImage)
		logs.GetLogger().Infof("Created deployment: %s", createDeployment.GetObjectMeta().GetName())

		if err := deployK8sResource(k8sNameSpace, spaceUuid, hostName, int64(cr.Ports[0].ContainerPort)); err != nil {
			logs.GetLogger().Error(err)
			return
		}
		updateJobStatus(jobUuid, models.JobDeployToK8s)

		watchContainerRunningTime(jobUuid, k8sNameSpace, spaceUuid, int64(duration))
	}
}

func deployNamespace(creatorWallet string) error {
	k8sNameSpace := constants.K8S_NAMESPACE_NAME_PREFIX + creatorWallet
	k8sService := NewK8sService()
	// create namespace
	if _, err := k8sService.GetNameSpace(context.TODO(), k8sNameSpace, metaV1.GetOptions{}); err != nil {
		if errors.IsNotFound(err) {
			namespace := &coreV1.Namespace{
				ObjectMeta: metaV1.ObjectMeta{
					Name: k8sNameSpace,
					Labels: map[string]string{
						"lab-ns": creatorWallet,
					},
				},
			}
			createdNamespace, err := k8sService.CreateNameSpace(context.TODO(), namespace, metaV1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed create namespace, error: %w", err)
			}
			logs.GetLogger().Infof("create namespace successfully, namespace: %s", createdNamespace.Name)

			//networkPolicy, err := k8sService.CreateNetworkPolicy(context.TODO(), k8sNameSpace)
			//if err != nil {
			//	return fmt.Errorf("failed create networkPolicy, error: %w", err)
			//}
			//logs.GetLogger().Infof("create networkPolicy successfully, networkPolicyName: %s", networkPolicy.Name)
		} else {
			return err
		}
	}
	return nil
}

func deployK8sResource(k8sNameSpace, spaceUuid, hostName string, containerPort int64) error {
	k8sService := NewK8sService()

	// create service
	createService, err := k8sService.CreateService(context.TODO(), k8sNameSpace, spaceUuid, int32(containerPort))
	if err != nil {
		return fmt.Errorf("failed creata service, error: %w", err)
	}
	logs.GetLogger().Infof("Created service successfully: %s", createService.GetObjectMeta().GetName())

	// create ingress
	createIngress, err := k8sService.CreateIngress(context.TODO(), k8sNameSpace, spaceUuid, hostName, int32(containerPort))
	if err != nil {
		return fmt.Errorf("failed creata ingress, error: %w", err)
	}
	logs.GetLogger().Infof("Created Ingress successfully: %s", createIngress.GetObjectMeta().GetName())
	return nil
}

func deleteJob(namespace, spaceUuid string) {
	deployName := constants.K8S_DEPLOY_NAME_PREFIX + spaceUuid
	serviceName := constants.K8S_SERVICE_NAME_PREFIX + spaceUuid
	ingressName := constants.K8S_INGRESS_NAME_PREFIX + spaceUuid

	k8sService := NewK8sService()
	if err := k8sService.DeleteIngress(context.TODO(), namespace, ingressName); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete ingress, ingressName: %s, error: %+v", deployName, err)
		return
	}
	logs.GetLogger().Infof("Deleted ingress %s finished", ingressName)

	if err := k8sService.DeleteService(context.TODO(), namespace, serviceName); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete service, serviceName: %s, error: %+v", serviceName, err)
		return
	}
	logs.GetLogger().Infof("Deleted service %s finished", serviceName)

	dockerService := docker.NewDockerService()
	deployImageIds, err := k8sService.GetDeploymentImages(context.TODO(), namespace, deployName)
	if err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed get deploy imageIds, deployName: %s, error: %+v", deployName, err)
		return
	}
	for _, imageId := range deployImageIds {
		dockerService.RemoveImage(imageId)
	}

	if err := k8sService.DeleteDeployment(context.TODO(), namespace, deployName); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete deployment, deployName: %s, error: %+v", deployName, err)
		return
	}
	time.Sleep(6 * time.Second)
	logs.GetLogger().Infof("Deleted deployment %s finished", deployName)

	if err := k8sService.DeleteDeployRs(context.TODO(), namespace, spaceUuid); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete ReplicaSetsController, spaceUuid: %s, error: %+v", spaceUuid, err)
		return
	}

	if err := k8sService.DeletePod(context.TODO(), namespace, spaceUuid); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete pods, spaceUuid: %s, error: %+v", spaceUuid, err)
		return
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var count = 0
	for {
		<-ticker.C
		count++
		if count >= 20 {
			break
		}
		getPods, err := k8sService.GetPods(namespace, spaceUuid)
		if err != nil && !errors.IsNotFound(err) {
			logs.GetLogger().Errorf("Failed get pods form namespace, namepace: %s, error: %+v", namespace, err)
			continue
		}
		if !getPods {
			logs.GetLogger().Infof("Deleted all resource finised. spaceUuid: %s", spaceUuid)
			break
		}
	}
}

func watchContainerRunningTime(key, namespace, spaceUuid string, runTime int64) {
	conn := redisPool.Get()
	_, err := conn.Do("SET", key, "wait-delete", "EX", runTime)
	if err != nil {
		logs.GetLogger().Errorf("Failed set redis key and expire time, key: %s, error: %+v", key, err)
		return
	}

	fullArgs := []interface{}{constants.REDIS_FULL_PREFIX + key}
	fields := map[string]string{
		"k8s_namespace": namespace,
		"expire_time":   strconv.Itoa(int(time.Now().Unix() + runTime)),
		"space_uuid":    spaceUuid,
	}

	for key, val := range fields {
		fullArgs = append(fullArgs, key, val)
	}
	conn.Do("HSET", fullArgs...)

	go func() {
		psc := redis.PubSubConn{Conn: redisPool.Get()}
		psc.PSubscribe("__keyevent@0__:expired")
		for {
			switch n := psc.Receive().(type) {
			case redis.Message:
				if n.Channel == "__keyevent@0__:expired" && string(n.Data) == key {
					logs.GetLogger().Infof("The namespace: %s, spaceUuid: %s, job has reached its runtime and will stop running.", namespace, spaceUuid)
					deleteJob(namespace, spaceUuid)
					redisPool.Get().Do("DEL", constants.REDIS_FULL_PREFIX+key)
				}
			case redis.Subscription:
				logs.GetLogger().Infof("Subscribe %s", n.Channel)
			case error:
				return
			}
		}
	}()
}

func updateJobStatus(jobUuid string, jobStatus models.JobStatus) {
	go func() {
		deployingChan <- models.Job{
			Uuid:   jobUuid,
			Status: jobStatus,
		}
	}()
}

func generateString(length int) string {
	characters := "abcdefghijklmnopqrstuvwxyz"
	numbers := "0123456789"
	source := characters + numbers
	result := make([]byte, length)
	rand.Seed(time.Now().UnixNano())
	for i := 0; i < length; i++ {
		result[i] = source[rand.Intn(len(source))]
	}
	return string(result)
}

func getLocation() (string, error) {
	publicIpAddress, err := getLocalIPAddress()
	if err != nil {
		return "", err
	}
	logs.GetLogger().Infof("publicIpAddress: %s", publicIpAddress)

	resp, err := http.Get("http://ip-api.com/json/" + publicIpAddress)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var ipInfo struct {
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		City        string `json:"city"`
		Region      string `json:"region"`
		RegionName  string `json:"regionName"`
	}
	if err = json.Unmarshal(body, &ipInfo); err != nil {
		return "", err
	}

	return strings.TrimSpace(ipInfo.RegionName) + "-" + ipInfo.CountryCode, nil
}

func getLocalIPAddress() (string, error) {
	req, err := http.NewRequest("GET", "https://ipapi.co/ip", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.212 Safari/537.36")

	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ipBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ipBytes)), nil
}

func getHardwareDetail(description string) models.Resource {
	var hardwareResource models.Resource
	confSplits := strings.Split(description, "·")
	if strings.Contains(confSplits[0], "CPU") {
		hardwareResource.Gpu.Quantity = 0
		hardwareResource.Gpu.Unit = ""
	} else {
		hardwareResource.Gpu.Quantity = 1
		oldName := strings.TrimSpace(confSplits[0])
		hardwareResource.Gpu.Unit = strings.ReplaceAll(oldName, "Nvidia", "NVIDIA")
	}

	cpuSplits := strings.Split(confSplits[1], " ")
	cores, _ := strconv.ParseInt(cpuSplits[1], 10, 64)
	hardwareResource.Cpu.Quantity = cores
	hardwareResource.Cpu.Unit = cpuSplits[2]

	memSplits := strings.Split(confSplits[2], " ")
	mem, _ := strconv.ParseInt(memSplits[1], 10, 64)
	hardwareResource.Memory.Quantity = mem
	hardwareResource.Memory.Unit = strings.ReplaceAll(memSplits[2], "B", "")

	hardwareResource.Storage.Quantity = 30
	hardwareResource.Storage.Unit = "Gi"
	return hardwareResource
}
