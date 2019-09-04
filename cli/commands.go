package run

/*
 * This file contains the "backend" functionality for the CLI commands (and flags)
 */

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/urfave/cli"
)

const (
	defaultRegistry    = "docker.io"
	defaultServerCount = 1
)

// CheckTools checks if the docker API server is responding
func CheckTools(c *cli.Context) error {
	log.Print("Checking docker...")
	ctx := context.Background()
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	ping, err := docker.Ping(ctx)

	if err != nil {
		return fmt.Errorf("ERROR: checking docker failed\n%+v", err)
	}
	log.Printf("SUCCESS: Checking docker succeeded (API: v%s)\n", ping.APIVersion)
	return nil
}

// CreateCluster creates a new single-node cluster container and initializes the cluster directory
func CreateCluster(c *cli.Context) error {

	// On Error delete the cluster.  If there createCluster() encounter any error,
	// call this function to remove all resources allocated for the cluster so far
	// so that they don't linger around.
	deleteCluster := func() {
		log.Println("ERROR: Cluster creation failed, rolling back...")
		if err := DeleteCluster(c); err != nil {
			log.Printf("Error: Failed to delete cluster %s", c.String("name"))
		}
	}

	/**********************
	 *										*
	 *		CONFIGURATION		*
	 * vvvvvvvvvvvvvvvvvv *
	 **********************/

	/*
	 * --name, -n
	 * Name of the cluster
	 */

	// ensure that it's a valid hostname, because it will be part of container names
	if err := CheckClusterName(c.String("name")); err != nil {
		return err
	}

	// check if the cluster name is already taken
	if cluster, err := getClusters(false, c.String("name")); err != nil {
		return err
	} else if len(cluster) != 0 {
		// A cluster exists with the same name. Return with an error.
		return fmt.Errorf("ERROR: Cluster %s already exists", c.String("name"))
	}

	/*
	 * --image, -i
	 * The k3s image used for the k3d node containers
	 */
	// define image
	image := c.String("image")
	// if no registry was provided, use the default docker.io
	if len(strings.Split(image, "/")) <= 2 {
		image = fmt.Sprintf("%s/%s", defaultRegistry, image)
	}

	/*
	 * Cluster network
	 * For proper communication, all k3d node containers have to be in the same docker network
	 */
	// create cluster network
	networkID, err := createClusterNetwork(c.String("name"))
	if err != nil {
		return err
	}
	log.Printf("Created cluster network with ID %s", networkID)

	/*
	 * --env, -e
	 * Environment variables that will be passed into the k3d node containers
	 */
	// environment variables
	env := []string{"K3S_KUBECONFIG_OUTPUT=/output/kubeconfig.yaml"}
	env = append(env, c.StringSlice("env")...)
	env = append(env, fmt.Sprintf("K3S_CLUSTER_SECRET=%s", GenerateRandomString(20)))

	/*
	 * Arguments passed on to the k3s server and agent, will be filled later
	 */
	k3AgentArgs := []string{}
	k3sServerArgs := []string{}

	/*
	 * --api-port, -a
	 * The port that will be used by the k3s API-Server
	 * It will be mapped to localhost or to another hist interface, if specified
	 * If another host is chosen, we also add a tls-san argument for the server to allow connections
	 */
	apiPort, err := parseAPIPort(c.String("api-port"))
	if err != nil {
		return err
	}
	k3sServerArgs = append(k3sServerArgs, "--https-listen-port", apiPort.Port)

	// When the 'host' is not provided by --api-port, try to fill it using Docker Machine's IP address.
	if apiPort.Host == "" {
		apiPort.Host, err = getDockerMachineIp()
		// IP address is the same as the host
		apiPort.HostIP = apiPort.Host
		// In case of error, Log a warning message, and continue on. Since it more likely caused by a miss configured
		// DOCKER_MACHINE_NAME environment variable.
		if err != nil {
			log.Printf("WARNING: Failed to get docker machine IP address, ignoring the DOCKER_MACHINE_NAME environment variable setting.\n")
		}
	}

	// Add TLS SAN for non default host name
	if apiPort.Host != "" {
		log.Printf("Add TLS SAN for %s", apiPort.Host)
		k3sServerArgs = append(k3sServerArgs, "--tls-san", apiPort.Host)
	}

	/*
	 * --server-arg, -x
	 * Add user-supplied arguments for the k3s server
	 */
	if c.IsSet("server-arg") || c.IsSet("x") {
		k3sServerArgs = append(k3sServerArgs, c.StringSlice("server-arg")...)
	}

	/*
	 * --agent-arg
	 * Add user-supplied arguments for the k3s agent
	 */
	if c.IsSet("agent-arg") {
		k3AgentArgs = append(k3AgentArgs, c.StringSlice("agent-arg")...)
	}

	/*
	 * --port, -p, --publish, --add-port
	 * List of ports, that should be mapped from some or all k3d node containers to the host system (or other interface)
	 */
	// new port map
	portmap, err := mapNodesToPortSpecs(c.StringSlice("port"), GetAllContainerNames(c.String("name"), defaultServerCount, c.Int("workers")))
	if err != nil {
		log.Fatal(err)
	}

	/*
	 * --volume, -v
	 * List of volumes: host directory mounts for some or all k3d node containers in the cluster
	 */
	volumes := c.StringSlice("volume")

	/*
	 * Image Volume
	 * A docker volume that will be shared by every k3d node container in the cluster.
	 * This volume will be used for the `import-image` command.
	 * On it, all node containers can access the image tarball.
	 */
	// create a docker volume for sharing image tarballs with the cluster
	imageVolume, err := createImageVolume(c.String("name"))
	log.Println("Created docker volume ", imageVolume.Name)
	if err != nil {
		return err
	}
	volumes = append(volumes, fmt.Sprintf("%s:/images", imageVolume.Name))

	/*
	 * clusterSpec
	 * Defines, with which specifications, the cluster and the nodes inside should be created
	 */
	clusterSpec := &ClusterSpec{
		AgentArgs:         k3AgentArgs,
		APIPort:           *apiPort,
		AutoRestart:       c.Bool("auto-restart"),
		ClusterName:       c.String("name"),
		Env:               env,
		Image:             image,
		NodeToPortSpecMap: portmap,
		PortAutoOffset:    c.Int("port-auto-offset"),
		ServerArgs:        k3sServerArgs,
		Verbose:           c.GlobalBool("verbose"),
		Volumes:           volumes,
	}

	/******************
	 *								*
	 *		CREATION		*
	 * vvvvvvvvvvvvvv	*
	 ******************/

	log.Printf("Creating cluster [%s]", c.String("name"))

	/*
	 * Cluster Directory
	 */
	// create the directory where we will put the kubeconfig file by default (when running `k3d get-config`)
	createClusterDir(c.String("name"))

	/* (1)
	 * Server
	 * Create the server node container
	 */
	serverContainerID, err := createServer(clusterSpec)
	if err != nil {
		deleteCluster()
		return err
	}

	/* (1.1)
	 * Wait
	 * Wait for k3s server to be done initializing, if wanted
	 */
	// We're simply scanning the container logs for a line that tells us that everything's up and running
	// TODO: also wait for worker nodes
	if c.IsSet("wait") {
		if err := waitForContainerLogMessage(serverContainerID, "Running kubelet", c.Int("wait")); err != nil {
			deleteCluster()
			return fmt.Errorf("ERROR: failed while waiting for server to come up\n%+v", err)
		}
	}

	/* (2)
	 * Workers
	 * Create the worker node containers
	 */
	// TODO: do this concurrently in different goroutines
	if c.Int("workers") > 0 {
		log.Printf("Booting %s workers for cluster %s", strconv.Itoa(c.Int("workers")), c.String("name"))
		for i := 0; i < c.Int("workers"); i++ {
			workerID, err := createWorker(clusterSpec, i)
			if err != nil {
				deleteCluster()
				return err
			}
			log.Printf("Created worker with ID %s\n", workerID)
		}
	}

	/* (3)
	 * Done
	 * Finished creating resources.
	 */
	log.Printf("SUCCESS: created cluster [%s]", c.String("name"))
	log.Printf(`You can now use the cluster with:

export KUBECONFIG="$(%s get-kubeconfig --name='%s')"
kubectl cluster-info`, os.Args[0], c.String("name"))

	return nil
}

// DeleteCluster removes the containers belonging to a cluster and its local directory
func DeleteCluster(c *cli.Context) error {
	clusters, err := getClusters(c.Bool("all"), c.String("name"))

	if err != nil {
		return err
	}

	// remove clusters one by one instead of appending all names to the docker command
	// this allows for more granular error handling and logging
	for _, cluster := range clusters {
		log.Printf("Removing cluster [%s]", cluster.name)
		if len(cluster.workers) > 0 {
			// TODO: this could be done in goroutines
			log.Printf("...Removing %d workers\n", len(cluster.workers))
			for _, worker := range cluster.workers {
				if err := removeContainer(worker.ID); err != nil {
					log.Println(err)
					continue
				}
			}
		}
		deleteClusterDir(cluster.name)
		log.Println("...Removing server")
		if err := removeContainer(cluster.server.ID); err != nil {
			return fmt.Errorf("ERROR: Couldn't remove server for cluster %s\n%+v", cluster.name, err)
		}

		if err := deleteClusterNetwork(cluster.name); err != nil {
			log.Printf("WARNING: couldn't delete cluster network for cluster %s\n%+v", cluster.name, err)
		}

		log.Println("...Removing docker image volume")
		if err := deleteImageVolume(cluster.name); err != nil {
			log.Printf("WARNING: couldn't delete image docker volume for cluster %s\n%+v", cluster.name, err)
		}

		log.Printf("SUCCESS: removed cluster [%s]", cluster.name)
	}

	return nil
}

// StopCluster stops a running cluster container (restartable)
func StopCluster(c *cli.Context) error {
	clusters, err := getClusters(c.Bool("all"), c.String("name"))

	if err != nil {
		return err
	}

	ctx := context.Background()
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("ERROR: couldn't create docker client\n%+v", err)
	}

	// remove clusters one by one instead of appending all names to the docker command
	// this allows for more granular error handling and logging
	for _, cluster := range clusters {
		log.Printf("Stopping cluster [%s]", cluster.name)
		if len(cluster.workers) > 0 {
			log.Printf("...Stopping %d workers\n", len(cluster.workers))
			for _, worker := range cluster.workers {
				if err := docker.ContainerStop(ctx, worker.ID, nil); err != nil {
					log.Println(err)
					continue
				}
			}
		}
		log.Println("...Stopping server")
		if err := docker.ContainerStop(ctx, cluster.server.ID, nil); err != nil {
			return fmt.Errorf("ERROR: Couldn't stop server for cluster %s\n%+v", cluster.name, err)
		}

		log.Printf("SUCCESS: Stopped cluster [%s]", cluster.name)
	}

	return nil
}

// StartCluster starts a stopped cluster container
func StartCluster(c *cli.Context) error {
	clusters, err := getClusters(c.Bool("all"), c.String("name"))

	if err != nil {
		return err
	}

	ctx := context.Background()
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("ERROR: couldn't create docker client\n%+v", err)
	}

	// remove clusters one by one instead of appending all names to the docker command
	// this allows for more granular error handling and logging
	for _, cluster := range clusters {
		log.Printf("Starting cluster [%s]", cluster.name)

		log.Println("...Starting server")
		if err := docker.ContainerStart(ctx, cluster.server.ID, types.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("ERROR: Couldn't start server for cluster %s\n%+v", cluster.name, err)
		}

		if len(cluster.workers) > 0 {
			log.Printf("...Starting %d workers\n", len(cluster.workers))
			for _, worker := range cluster.workers {
				if err := docker.ContainerStart(ctx, worker.ID, types.ContainerStartOptions{}); err != nil {
					log.Println(err)
					continue
				}
			}
		}

		log.Printf("SUCCESS: Started cluster [%s]", cluster.name)
	}

	return nil
}

// ListClusters prints a list of created clusters
func ListClusters(c *cli.Context) error {
	if c.IsSet("all") {
		log.Println("INFO: --all is on by default, thus no longer required. This option will be removed in v2.0.0")

	}
	printClusters()
	return nil
}

// GetKubeConfig grabs the kubeconfig from the running cluster and prints the path to stdout
func GetKubeConfig(c *cli.Context) error {
	cluster := c.String("name")
	kubeConfigPath, err := getKubeConfig(cluster)
	if err != nil {
		return err
	}

	// output kubeconfig file path to stdout
	fmt.Println(kubeConfigPath)
	return nil
}

// Shell starts a new subshell with the KUBECONFIG pointing to the selected cluster
func Shell(c *cli.Context) error {
	return subShell(c.String("name"), c.String("shell"), c.String("command"))
}

// ImportImage saves an image locally and imports it into the k3d containers
func ImportImage(c *cli.Context) error {
	images := make([]string, 0)
	if strings.Contains(c.Args().First(), ",") {
		images = append(images, strings.Split(c.Args().First(), ",")...)
	} else {
		images = append(images, c.Args()...)
	}
	return importImage(c.String("name"), images, c.Bool("no-remove"))
}

// AddNode adds a node to an existing cluster
func AddNode(c *cli.Context) error {

	/*
	 * (0) Check flags
	 */

	clusterName := c.String("name")
	nodeCount := c.Int("count")

	/* (0.1)
	 * --role
	 * Role of the node that has to be created.
	 * One of (server|master), (agent|worker)
	 */
	nodeRole := c.String("role")
	if nodeRole == "worker" {
		nodeRole = "agent"
	}
	if nodeRole == "master" {
		nodeRole = "server"
	}

	// TODO: support this
	if nodeRole == "server" {
		return fmt.Errorf("ERROR: sorry, we don't support adding server nodes at the moment!")
	}

	/* (0.2)
	 * --image, -i
	 * The k3s image used for the k3d node containers
	 */
	image := c.String("image")
	// if no registry was provided, use the default docker.io
	if len(strings.Split(image, "/")) <= 2 {
		image = fmt.Sprintf("%s/%s", defaultRegistry, image)
	}

	/*
	 * (1) Check cluster
	 */

	ctx := context.Background()
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("ERROR: couldn't create docker client\n%+v", err)
	}

	filters := filters.NewArgs()
	filters.Add("label", fmt.Sprintf("cluster=%s", clusterName))
	filters.Add("label", "app=k3d")

	/*
	 * (1.1) Verify, that the cluster (i.e. the server) that we want to connect to, is running
	 */
	filters.Add("label", "component=server")

	serverList, err := docker.ContainerList(ctx, types.ContainerListOptions{
		Filters: filters,
	})
	if err != nil || len(serverList) == 0 {
		return fmt.Errorf("ERROR: coudln't get server container for cluster %s\n%+v", clusterName, err)
	}

	/*
	 * (1.2) Extract cluster information from server container
	 */
	serverContainer, err := docker.ContainerInspect(ctx, serverList[0].ID)
	if err != nil {
		return fmt.Errorf("ERROR: couldn't inspect server container [%s] to get cluster secret\n%+v", serverList[0].ID, err)
	}

	/*
	 * (1.2.1) Extract cluster secret from server container's labels
	 */
	clusterSecretEnvVar := ""
	for _, envVar := range serverContainer.Config.Env {
		if envVarSplit := strings.SplitN(envVar, "=", 2); envVarSplit[0] == "K3S_CLUSTER_SECRET" {
			clusterSecretEnvVar = envVar
		}
	}
	if clusterSecretEnvVar == "" {
		return fmt.Errorf("ERROR: couldn't get cluster secret from server container")
	}

	/*
	 * (1.2.2) Extract API server Port from server container's cmd
	 */
	serverListenPort := ""
	for cmdIndex, cmdPart := range serverContainer.Config.Cmd {
		if cmdPart == "--https-listen-port" {
			serverListenPort = serverContainer.Config.Cmd[cmdIndex+1]
		}
	}
	if serverListenPort == "" {
		return fmt.Errorf("ERROR: couldn't get https-listen-port form server contaienr")
	}

	/*
	 * (1.3) Get the docker network of the cluster that we want to connect to
	 */
	filters.Del("label", "component=server")

	networkList, err := docker.NetworkList(ctx, types.NetworkListOptions{
		Filters: filters,
	})
	if err != nil || len(networkList) == 0 {
		return fmt.Errorf("ERROR: couldn't find network for cluster %s\n%+v", clusterName, err)
	}

	/*
	 * (2) Now identify any existing worker nodes IF we're adding a new one
	 */
	highestExistingWorkerSuffix := 0 // needs to be outside conditional because of bad branching

	if nodeRole == "agent" {
		filters.Add("label", "component=worker")

		workerList, err := docker.ContainerList(ctx, types.ContainerListOptions{
			Filters: filters,
			All:     true,
		})
		if err != nil {
			return fmt.Errorf("ERROR: couldn't list worker node containers\n%+v", err)
		}

		for _, worker := range workerList {
			split := strings.Split(worker.Names[0], "-")
			currSuffix, err := strconv.Atoi(split[len(split)-1])
			if err != nil {
				return fmt.Errorf("ERROR: failed to get highest worker suffix\n%+v", err)
			}
			if currSuffix > highestExistingWorkerSuffix {
				highestExistingWorkerSuffix = currSuffix
			}
		}
	}

	/*
	 * (3) Create the nodes with configuration that automatically joins them to the cluster
	 */

	serverURLEnvVar := fmt.Sprintf("K3S_URL=https://%s:%s", serverContainer.Name, serverListenPort)

	env := []string{}

	env = append(env, serverURLEnvVar)
	env = append(env, clusterSecretEnvVar)

	clusterSpec := &ClusterSpec{
		AgentArgs:         nil,
		APIPort:           apiPort{},
		AutoRestart:       false,
		ClusterName:       clusterName,
		Env:               env,
		Image:             image,
		NodeToPortSpecMap: nil,
		PortAutoOffset:    0,
		ServerArgs:        nil,
		Verbose:           false,
		Volumes:           nil,
	}

	log.Printf("INFO: Adding %d %s-nodes to cluster %s...\n", nodeCount, nodeRole, clusterName)

	if nodeRole == "agent" {
		for suffix := highestExistingWorkerSuffix + 1; suffix < nodeCount+1+highestExistingWorkerSuffix; suffix++ {
			workerContainerID, err := createWorker(clusterSpec, suffix)
			if err != nil {
				return fmt.Errorf("ERROR: Couldn't create %s-node!\n%+v", nodeRole, err)
			}
			log.Printf("INFO: Created %s-node with ID %s\n", nodeRole, workerContainerID)
		}
	}

	return nil
}