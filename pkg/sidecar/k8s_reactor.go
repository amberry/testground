//+build linux

package sidecar

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/ipfs/testground/pkg/docker"
	"github.com/ipfs/testground/pkg/logging"
	"github.com/ipfs/testground/sdk/runtime"

	"github.com/containernetworking/cni/libcni"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	controlNetworkIfname = "eth0"
	dataNetworkIfname    = "eth1"
	podCidr              = "100.96.0.0/11"
)

var (
	kubeDnsClusterIP = net.IPv4(100, 64, 0, 10)
)

type K8sReactor struct {
	redis   net.IP
	manager *docker.Manager
}

func NewK8sReactor() (Reactor, error) {
	redisHost := os.Getenv(EnvRedisHost)

	redisIp, err := net.ResolveIPAddr("ip4", redisHost)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve redis: %w", err)
	}

	docker, err := docker.NewManager()
	if err != nil {
		return nil, err
	}

	return &K8sReactor{
		manager: docker,
		redis:   redisIp.IP,
	}, nil
}

func (d *K8sReactor) Handle(ctx context.Context, handler InstanceHandler) error {
	return d.manager.Watch(ctx, func(cctx context.Context, container *docker.ContainerRef) error {
		logging.S().Debugw("got container", "container", container.ID)
		inst, err := d.manageContainer(cctx, container)
		if err != nil {
			return fmt.Errorf("failed to initialise the container: %w", err)
		}
		if inst == nil {
			logging.S().Debugw("ignoring container", "container", container.ID)
			return nil
		}
		logging.S().Debugw("managing container", "container", container.ID)
		err = handler(cctx, inst)
		if err != nil {
			return fmt.Errorf("container worker failed: %w", err)
		}
		return nil
	})
}

func (d *K8sReactor) Close() error {
	return d.manager.Close()
}

func (d *K8sReactor) manageContainer(ctx context.Context, container *docker.ContainerRef) (inst *Instance, err error) {
	// Get the state/config of the cluster
	info, err := container.Inspect(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect failed: %w", err)
	}

	if !info.State.Running {
		return nil, fmt.Errorf("container is not running: %s", container.ID)
	}

	// Construct the runtime environment
	params, err := runtime.ParseRunParams(info.Config.Env)
	if err != nil {
		return nil, fmt.Errorf("failed to parse run environment: %w", err)
	}

	if !params.TestSidecar {
		return nil, nil
	}

	podName, ok := info.Config.Labels["io.kubernetes.pod.name"]
	if !ok {
		return nil, fmt.Errorf("couldn't get pod name from container labels for: %s", container.ID)
	}

	err = waitForPodRunningPhase(ctx, podName)
	if err != nil {
		return nil, err
	}

	// Remove the TestOutputsPath. We can't store anything from the sidecar.
	params.TestOutputsPath = ""
	runenv := runtime.NewRunEnv(*params)

	//////////////////
	//  NETWORKING  //
	//////////////////

	// Initialise CNI config
	cninet := libcni.NewCNIConfig(filepath.SplitList("/host/opt/cni/bin"), nil)

	// Get a netlink handle.
	nshandle, err := netns.GetFromPid(info.State.Pid)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup the net namespace: %s", err)
	}
	defer nshandle.Close()

	netlinkHandle, err := netlink.NewHandleAt(nshandle)
	if err != nil {
		return nil, fmt.Errorf("failed to get handle to network namespace: %w", err)
	}

	defer func() {
		if err != nil {
			netlinkHandle.Delete()
		}
	}()

	// Finally, construct the network manager.
	network := &K8sNetwork{
		netnsPath:   fmt.Sprintf("/proc/%d/ns/net", info.State.Pid),
		cninet:      cninet,
		container:   container,
		subnet:      runenv.TestSubnet.String(),
		nl:          netlinkHandle,
		activeLinks: make(map[string]*k8sLink),
	}

	// Remove all routes but redis and the data subnet

	// We've found a control network (or some other network).
	controlLink, err := netlinkHandle.LinkByName(controlNetworkIfname)
	if err != nil {
		return nil, fmt.Errorf("failed to get link by name %s: %w", controlNetworkIfname, err)
	}

	// Get the routes to redis. We need to keep these.
	redisRoute, err := getRedisRoute(netlinkHandle, d.redis)
	if err != nil {
		return nil, fmt.Errorf("cant get route to redis: %s", err)
	}
	logging.S().Debugw("got redis route", "route.Src", redisRoute.Src, "route.Dst", redisRoute.Dst, "gw", redisRoute.Gw.String(), "container", container.ID)

	controlLinkRoutes, err := netlinkHandle.RouteList(controlLink, netlink.FAMILY_ALL)
	if err != nil {
		return nil, fmt.Errorf("failed to list routes for control link %s", controlLink.Attrs().Name)
	}

	redisIP := redisRoute.Dst.IP

	routesToBeDeleted := []netlink.Route{}

	// Remove the original routes
	for _, route := range controlLinkRoutes {
		routeDst := "nil"
		if route.Dst != nil {
			routeDst = route.Dst.String()
		}

		logging.S().Debugw("inspecting controlLink route", "route.Src", route.Src, "route.Dst", routeDst, "gw", route.Gw, "container", container.ID)

		if route.Dst != nil && route.Dst.String() == podCidr {
			logging.S().Debugw("marking for deletion podCidr dst route", "route.Src", route.Src, "route.Dst", routeDst, "gw", route.Gw, "container", container.ID)
			routesToBeDeleted = append(routesToBeDeleted, route)
			continue
		}

		if route.Dst != nil {
			if route.Dst.Contains(redisIP) {
				newroute := route
				newroute.Dst = &net.IPNet{
					IP:   redisIP,
					Mask: net.CIDRMask(32, 32),
				}

				logging.S().Debugw("adding redis route", "route.Src", newroute.Src, "route.Dst", newroute.Dst.String(), "gw", newroute.Gw, "container", container.ID)
				if err := netlinkHandle.RouteAdd(&newroute); err != nil {
					logging.S().Debugw("failed to add route while restricting gw route", "container", container.ID, "err", err.Error())
				} else {
					logging.S().Debugw("successfully added route", "route.Src", newroute.Src, "route.Dst", newroute.Dst.String(), "gw", newroute.Gw, "container", container.ID)
				}
			}

			logging.S().Debugw("marking for deletion some dst route", "route.Src", route.Src, "route.Dst", routeDst, "gw", route.Gw, "container", container.ID)
			routesToBeDeleted = append(routesToBeDeleted, route)
			continue
		}

		logging.S().Debugw("marking for deletion random route", "route.Src", route.Src, "route.Dst", routeDst, "gw", route.Gw, "container", container.ID)
		routesToBeDeleted = append(routesToBeDeleted, route)
	}

	// Adding DNS route
	for _, route := range controlLinkRoutes {
		if route.Dst == nil && route.Src == nil {
			// if default route, get the gw and add a route for DNS
			dnsRoute := route
			dnsRoute.Src = nil
			dnsRoute.Dst = &net.IPNet{
				IP:   kubeDnsClusterIP,
				Mask: net.CIDRMask(32, 32),
			}

			logging.S().Debugw("adding dns route", "container", container.ID)
			if err := netlinkHandle.RouteAdd(&dnsRoute); err != nil {
				return nil, fmt.Errorf("failed to add dns route to pod: %v", err)
			}
		}
	}

	for _, r := range routesToBeDeleted {
		routeDst := "nil"
		if r.Dst != nil {
			routeDst = r.Dst.String()
		}

		logging.S().Debugw("really removing route", "route.Src", r.Src, "route.Dst", routeDst, "gw", r.Gw, "container", container.ID)
		if err := netlinkHandle.RouteDel(&r); err != nil {
			logging.S().Warnw("failed to really delete route", "route.Src", r.Src, "gw", r.Gw, "route.Dst", routeDst, "container", container.ID, "err", err.Error())
		}
	}

	return NewInstance(ctx, runenv, info.Config.Hostname, network)
}

func waitForPodRunningPhase(ctx context.Context, podName string) error {
	k8scfg, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		return fmt.Errorf("error in wait for pod running phase: %v", err)
	}

	k8sClientset, err := kubernetes.NewForConfig(k8scfg)
	if err != nil {
		return fmt.Errorf("error in wait for pod running phase: %v", err)
	}

	var phase string

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for pod context (pod name: %s) erred with: %w", podName, ctx.Err())
		default:
			if phase == "Running" {
				return nil
			}
			pod, err := k8sClientset.CoreV1().Pods("default").Get(podName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("error in wait for pod running phase: %v", err)
			}

			phase = string(pod.Status.Phase)

			time.Sleep(1 * time.Second)
		}
	}
}
