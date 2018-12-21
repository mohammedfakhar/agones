# Scheduling and Autoscaling

> Autoscaling is currently ongoing work within Agones. The work you see here is just the beginning.

Table of Contents
=================

  * [Kubernetes Cluster Autoscaler](#kubernetes-cluster-autoscaler)
     * [Google Kubernetes Engine](#google-kubernetes-engine)
     * [Azure Kubernetes Service](#azure-kubernetes-service)
  * [Fleet Autoscaling](#fleet-autoscaling)
  * [Autoscaling Concepts](#autoscaling-concepts)
     * [Allocation Scheduling](#allocation-scheduling)
     * [Pod Scheduling](#pod-scheduling)
     * [Fleet Scale Down Strategy](#fleet-scale-down-strategy)
  * [Fleet Scheduling](#fleet-scheduling)
     * [Packed](#packed)
        * [Allocation Scheduling Strategy](#allocation-scheduling-strategy)
        * [Pod Scheduling Strategy](#pod-scheduling-strategy)
        * [Fleet Scale Down Strategy](#fleet-scale-down-strategy-1)
     * [Distributed](#distributed)
        * [Allocation Scheduling Strategy](#allocation-scheduling-strategy-1)
        * [Pod Scheduling Strategy](#pod-scheduling-strategy-1)
        * [Fleet Scale Down Strategy](#fleet-scale-down-strategy-2)

Scheduling and autoscaling go hand in hand, as where in the cluster `GameServers` are provisioned
impacts how to autoscale fleets up and down (or if you would even want to)

## Cluster Autoscaler

Kubernetes has a [cluster node autoscaler that works with a wide variety of cloud providers](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler).

The default scheduling strategy (`Packed`) is designed to work with the Kubernetes autoscaler out of the box.

The autoscaler will automatically add Nodes to the cluster when `GameServers` don't have room to be scheduled on the
clusters, and then scale down when there are empty Nodes with no `GameServers` running on them.

This means that scaling `Fleets` up and down can be used to control the size of the cluster, as the cluster autoscaler
will adjust the size of the cluster to match the resource needs of one or more `Fleets` running on it.

To enable and configure autoscaling on your cloud provider, check their [connector implementation](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider),
or their cloud specific documentation.

### Google Kubernetes Engine
* [Administering Clusters: Autoscaling a Cluster](https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-autoscaler)
* [Cluster Autoscaler](https://cloud.google.com/kubernetes-engine/docs/concepts/cluster-autoscaler)

### Azure Kubernetes Service
* [Cluster Autoscaler on Azure Kubernetes Service (AKS) - Preview](https://docs.microsoft.com/en-us/azure/aks/autoscaler)

## Fleet Autoscaling

Fleet autoscaling is the only type of autoscaling that exists in Agones. It is currently only available as a simple
buffer autoscaling strategy. Have a look at the [Create a Fleet Autoscaler](create_fleetautoscaler.md) quickstart,
and the [Fleet Autoscaler Specification](fleetautoscaler_spec.md) for details.

More sophisticated fleet autoscaling will be coming in future releases.

## Autoscaling Concepts

To facilitate autoscaling, we need to combine several piece of concepts and functionality, described below.

### Allocation Scheduling

Allocation scheduling refers to the order in which `GameServers`, and specifically their backing `Pods` are chosen
from across the Kubernetes cluster within a given `Fleet` when [allocation](./create_fleet.md#4-allocate-a-game-server-from-the-fleet) occurs.

### Pod Scheduling

Each `GameServer` is backed by a Kubernetes [`Pod`](https://kubernetes.io/docs/concepts/workloads/pods/pod/). Pod scheduling
refers to the strategy that is in place that determines which node in the Kubernetes cluster the Pod is assigned to,
when it is created.

### Fleet Scale Down Strategy

Fleet Scale Down strategy refers to the order in which the `GameServers` that belong to a `Fleet` are deleted, 
when Fleets are shrunk in size.

## Fleet Scheduling

There are two scheduling strategies for Fleets - each designed for different types of Kubernetes Environments.

### Packed

```yaml
apiVersion: "stable.agones.dev/v1alpha1"
kind: Fleet
metadata:
  name: simple-udp
spec:
  replicas: 100
  scheduling: Packed
  template:
    spec:
      ports:
      - containerPort: 7654
      template:
        spec:
          containers:
          - name: simple-udp
            image: gcr.io/agones-images/udp-server:0.5
```

This is the *default* Fleet scheduling strategy. It is designed for dynamic Kubernetes environments, wherein you wish 
to scale up and down as load increases or decreases, such as in a Cloud environment where you are paying
for the infrastructure you use.

It attempts to _pack_ as much as possible into the smallest set of nodes, to make
scaling infrastructure down as easy as possible.

This affects the Cluster autoscaler, Allocation Scheduling, Pod Scheduling and Fleet Scale Down Scheduling.

#### Cluster Autoscaler

To ensure that the Cluster Autoscaler doesn't attempt to evict and move `GameServer` `Pods` onto new Nodes during
gameplay, Agones adds the annotation [`"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"`](https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/FAQ.md#what-types-of-pods-can-prevent-ca-from-removing-a-node)
to the backing Pod.

#### Allocation Scheduling Strategy

Under the "Packed" strategy, allocation will prioritise allocating `GameServers` to nodes that are running on 
Nodes that already have allocated `GameServers` running on them.

#### Pod Scheduling Strategy

Under the "Packed" strategy, Pods will be scheduled using the [`PodAffinity`](https://kubernetes.io/docs/concepts/configuration/assign-pod-node/#inter-pod-affinity-and-anti-affinity-beta-feature)
with a `preferredDuringSchedulingIgnoredDuringExecution` affinity with [hostname](https://kubernetes.io/docs/concepts/configuration/assign-pod-node/#interlude-built-in-node-labels)
topology. This attempts to group together `GameServer` Pods within as few nodes in the cluster as it can.

> The default Kubernetes scheduler doesn't do a perfect job of packing, but it's a good enough job for what we need - 
  at least at this stage. 

#### Fleet Scale Down Strategy

With the "Packed" strategy, Fleets will remove `Ready` `GameServers` from Nodes with the _least_ number of `Ready` and 
`Allocated` `GameServers` on them. Attempting to empty Nodes so that they can be safely removed.

### Distributed

```yaml
apiVersion: "stable.agones.dev/v1alpha1"
kind: Fleet
metadata:
  name: simple-udp
spec:
  replicas: 100
  scheduling: Distributed
  template:
    spec:
      ports:
      - containerPort: 7654
      template:
        spec:
          containers:
          - name: simple-udp
            image: gcr.io/agones-images/udp-server:0.5
```

This Fleet scheduling strategy is designed for static Kubernetes environments, such as when you are running Kubernetes
on bare metal, and the cluster size rarely changes, if at all.

This attempts to distribute the load across the entire cluster as much as possible, to take advantage of the static
size of the cluster.

This affects Allocation Scheduling, Pod Scheduling and Fleet Scale Down Scheduling.

#### Cluster Autoscaler

Since this strategy is not aimed at clusters that autoscale, this strategy does nothing for the cluster autoscaler.

#### Allocation Scheduling Strategy

Under the "Distributed" strategy, allocation will prioritise allocating `GameSerers` to nodes that have the least
number of allocated `GameServers` on them.

#### Pod Scheduling Strategy

Under the "Distributed" strategy, `Pod` scheduling is provided by the default Kubernetes scheduler, which will attempt
to distribute the `GameServer` `Pods` across as many nodes as possible.

#### Fleet Scale Down Strategy

With the "Distributed" strategy, Fleets will remove `Ready` `GameServers` from Nodes with at random, to ensure
a distributed load is maintained.