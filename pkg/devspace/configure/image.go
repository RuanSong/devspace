package configure

import (
	contextpkg "context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/devspace-cloud/devspace/pkg/devspace/build/builder/helper"
	"github.com/devspace-cloud/devspace/pkg/devspace/config/versions/latest"
	v1 "github.com/devspace-cloud/devspace/pkg/devspace/config/versions/latest"
	"github.com/devspace-cloud/devspace/pkg/devspace/docker"
	"github.com/devspace-cloud/devspace/pkg/devspace/registry"
	"github.com/devspace-cloud/devspace/pkg/util/ptr"
	"github.com/devspace-cloud/devspace/pkg/util/survey"
	"github.com/pkg/errors"
)

const dockerHubHostname = "hub.docker.com"
const noRegistryImage = "devspace"

// newImageConfigFromImageName returns an image config based on the image
func (m *manager) newImageConfigFromImageName(imageName, dockerfile, context string) *latest.ImageConfig {
	// Figure out tag
	imageTag := ""
	splittedImage := strings.Split(imageName, ":")
	imageName = splittedImage[0]
	if len(splittedImage) > 1 {
		imageTag = splittedImage[1]
	} else if dockerfile == "" {
		imageTag = "latest"
	}

	retImageConfig := &latest.ImageConfig{
		Image:            imageName,
		CreatePullSecret: ptr.Bool(true),
	}

	if imageTag != "" {
		retImageConfig.Tags = []string{imageTag}
	}
	if dockerfile == "" {
		retImageConfig.Build = &latest.BuildConfig{
			Disabled: ptr.Bool(true),
		}
	} else {
		if dockerfile != helper.DefaultDockerfilePath {
			retImageConfig.Dockerfile = dockerfile
		}
		if context != "" && context != helper.DefaultContextPath {
			retImageConfig.Context = context
		}
	}

	return retImageConfig
}

// newImageConfigFromDockerfile gets the image config based on the configured cloud provider or asks the user where to push to
func (m *manager) newImageConfigFromDockerfile(imageName, dockerfile, context string) (*latest.ImageConfig, error) {
	var (
		dockerUsername = ""
		retImageConfig = &latest.ImageConfig{
			InjectRestartHelper:   true,
			PreferSyncOverRebuild: true,
			AppendDockerfileInstructions: []string{
				"USER root",
			},
		}
	)

	// Ignore error as context may not be a Space
	kubeContext, err := m.factory.NewKubeConfigLoader().GetCurrentContext()
	if err != nil {
		return nil, err
	}

	// Get docker client
	dockerClient, err := m.factory.NewDockerClientWithMinikube(kubeContext, true, m.log)
	if err != nil {
		return nil, errors.Errorf("Cannot create docker client: %v", err)
	}

	// Check if docker is installed
	_, err = dockerClient.Ping(contextpkg.Background())
	if err != nil {
		// Check if docker cli is installed
		runErr := exec.Command("docker").Run()
		if runErr == nil {
			m.log.Warn("Docker daemon not running. Start Docker daemon to build images with Docker instead of using the kaniko fallback.")
		}
	}

	registryURL, err := m.getRegistryURL(dockerClient)
	if err != nil {
		return nil, err
	}

	if registryURL == "" {
		imageName = noRegistryImage
	} else if registryURL == "hub.docker.com" {
		m.log.StartWait("Checking Docker credentials")
		dockerAuthConfig, err := dockerClient.GetAuthConfig("", true)
		m.log.StopWait()
		if err == nil {
			dockerUsername = dockerAuthConfig.Username
		}

		imageName, err = m.log.Question(&survey.QuestionOptions{
			Question:          "Which image name do you want to use on Docker Hub?",
			DefaultValue:      dockerUsername + "/" + imageName,
			ValidationMessage: "Please enter a valid image name for Docker Hub (e.g. myregistry.com/user/repository | allowed charaters: /, a-z, 0-9)",
			ValidationFunc: func(name string) error {
				_, err := registry.GetStrippedDockerImageName(name)
				return err
			},
		})
		if err != nil {
			return nil, err
		}

		imageName, _ = registry.GetStrippedDockerImageName(imageName)
	} else if regexp.MustCompile("^(.+\\.)?gcr.io$").Match([]byte(registryURL)) { // Is google registry?
		project, err := exec.Command("gcloud", "config", "get-value", "project").Output()
		gcloudProject := "myGCloudProject"

		if err == nil {
			gcloudProject = strings.TrimSpace(string(project))
		}

		imageName, err = m.log.Question(&survey.QuestionOptions{
			Question:          "Which image name do you want to push to?",
			DefaultValue:      registryURL + "/" + gcloudProject + "/" + imageName,
			ValidationMessage: "Please enter a valid Docker image name (e.g. myregistry.com/user/repository | allowed charaters: /, a-z, 0-9)",
			ValidationFunc: func(name string) error {
				_, err := registry.GetStrippedDockerImageName(name)
				return err
			},
		})
		if err != nil {
			return nil, err
		}

		imageName, _ = registry.GetStrippedDockerImageName(imageName)
	} else {
		if dockerUsername == "" {
			dockerUsername = "myuser"
		}

		imageName, err = m.log.Question(&survey.QuestionOptions{
			Question:          "Which image name do you want to push to?",
			DefaultValue:      registryURL + "/" + dockerUsername + "/" + imageName,
			ValidationMessage: "Please enter a valid docker image name (e.g. myregistry.com/user/repository)",
			ValidationFunc: func(name string) error {
				_, err := registry.GetStrippedDockerImageName(name)
				return err
			},
		})
		if err != nil {
			return nil, err
		}

		imageName, _ = registry.GetStrippedDockerImageName(imageName)
	}

	targets, err := helper.GetDockerfileTargets(dockerfile)
	if err != nil {
		return nil, err
	}

	if len(targets) > 0 {
		targetNone := "[none] (build complete Dockerfile)"
		targets = append(targets, targetNone)
		target, err := m.log.Question(&survey.QuestionOptions{
			Question: "Which build stage (target) within your Dockerfile do you want to use for development?\n  Choose `build` for quickstart projects.",
			Options:  targets,
		})
		if err != nil {
			return nil, err
		}

		if target != targetNone {
			retImageConfig.Build = &latest.BuildConfig{
				Docker: &latest.DockerConfig{
					Options: &latest.BuildOptions{
						Target: target,
					},
				},
			}
		}
	}

	// Set image name
	retImageConfig.Image = imageName

	// Set image specifics
	if dockerfile != helper.DefaultDockerfilePath {
		retImageConfig.Dockerfile = dockerfile
	}
	if context != "" && context != helper.DefaultContextPath {
		retImageConfig.Context = context
	}
	if imageName == noRegistryImage {
		if retImageConfig.Build == nil {
			retImageConfig.Build = &v1.BuildConfig{}
		}
		if retImageConfig.Build.Docker == nil {
			retImageConfig.Build.Docker = &v1.DockerConfig{}
		}

		retImageConfig.Build.Docker.SkipPush = ptr.Bool(true)
	}

	return retImageConfig, nil
}

func (m *manager) getRegistryURL(dockerClient docker.Client) (string, error) {
	var (
		useDockerHub          = "Use " + dockerHubHostname
		skipImagePush         = "Always skip image push (advanced, config will not work with remote clusters)"
		useOtherRegistry      = "Use other registry"
		registryUsernameHint  = " => you are logged in as %s"
		registryDefaultOption = useDockerHub
		registryLoginHint     = "Please login via `docker login%s` and try again."
	)

	authConfig, err := dockerClient.GetAuthConfig(dockerHubHostname, true)
	if err == nil && authConfig.Username != "" {
		useDockerHub = useDockerHub + fmt.Sprintf(registryUsernameHint, authConfig.Username)
		registryDefaultOption = useDockerHub
	}

	registryOptions := []string{useDockerHub, useOtherRegistry}
	registryOptions = append(registryOptions, skipImagePush)
	selectedRegistry, err := m.log.Question(&survey.QuestionOptions{
		Question:     "Which registry do you want to use for storing your Docker images?",
		DefaultValue: registryDefaultOption,
		Options:      registryOptions,
	})
	if err != nil {
		return "", err
	}

	var registryURL string
	if selectedRegistry == skipImagePush {
		return "", nil
	} else if selectedRegistry == useDockerHub {
		registryURL = dockerHubHostname
	} else {
		registryURL, err = m.log.Question(&survey.QuestionOptions{
			Question:     "Please enter the registry URL without image name:",
			DefaultValue: "my.registry.tld/username",
		})
		if err != nil {
			return "", err
		}

		registryURL = strings.Trim(registryURL, "/ ")
		registryLoginHint = fmt.Sprintf(registryLoginHint, " "+registryURL)
	}

	m.log.StartWait("Checking registry authentication")
	authConfig, err = dockerClient.Login(registryURL, "", "", true, false, false)
	m.log.StopWait()
	if err != nil || authConfig.Username == "" {
		if registryURL == dockerHubHostname {
			m.log.Warn("You are not logged in to Docker Hub")
			m.log.Warn("Please make sure you have a https://hub.docker.com account")
			m.log.Warn("Installing docker is NOT required. You simply need a Docker Hub account\n")

			for {
				dockerUsername, err := m.log.Question(&survey.QuestionOptions{
					Question:               "What is your Docker Hub username?",
					DefaultValue:           "",
					ValidationRegexPattern: "^.*$",
				})
				if err != nil {
					return "", err
				}

				dockerPassword, err := m.log.Question(&survey.QuestionOptions{
					Question:               "What is your Docker Hub password? (will only be sent to Docker Hub)",
					DefaultValue:           "",
					ValidationRegexPattern: "^.*$",
					IsPassword:             true,
				})
				if err != nil {
					return "", err
				}

				_, err = dockerClient.Login(registryURL, dockerUsername, dockerPassword, false, true, true)
				if err != nil {
					m.log.Warn(err)
					continue
				}

				break
			}
		} else {
			return "", errors.Errorf("Registry authentication failed for %s.\n         %s", registryURL, registryLoginHint)
		}
	}

	return registryURL, nil
}

// AddImage adds an image to the devspace
func (m *manager) AddImage(nameInConfig, name, tag, contextPath, dockerfilePath, buildTool string) error {
	imageConfig := &v1.ImageConfig{
		Image: name,
	}

	if tag != "" {
		imageConfig.Tags = []string{tag}
	}
	if contextPath != "" {
		imageConfig.Context = contextPath
	}
	if dockerfilePath != "" {
		imageConfig.Dockerfile = dockerfilePath
	}

	if buildTool == "docker" {
		if imageConfig.Build == nil {
			imageConfig.Build = &v1.BuildConfig{}
		}

		imageConfig.Build.Docker = &v1.DockerConfig{}
	} else if buildTool == "kaniko" {
		if imageConfig.Build == nil {
			imageConfig.Build = &v1.BuildConfig{}
		}

		imageConfig.Build.Kaniko = &v1.KanikoConfig{}
	} else if buildTool != "" {
		m.log.Errorf("BuildTool %v unknown. Please select one of docker|kaniko", buildTool)
	}

	if m.config.Images == nil {
		images := make(map[string]*v1.ImageConfig)
		m.config.Images = images
	}

	m.config.Images[nameInConfig] = imageConfig

	return nil
}

//RemoveImage removes an image from the devspace
func (m *manager) RemoveImage(removeAll bool, names []string) error {
	if len(names) == 0 && removeAll == false {
		return errors.Errorf("You have to specify at least one image")
	}

	newImageList := make(map[string]*v1.ImageConfig)

	if !removeAll && m.config.Images != nil {
	ImagesLoop:
		for nameInConfig, imageConfig := range m.config.Images {
			for _, deletionName := range names {
				if deletionName == nameInConfig {
					continue ImagesLoop
				}
			}

			newImageList[nameInConfig] = imageConfig
		}
	}

	m.config.Images = newImageList

	return nil
}
