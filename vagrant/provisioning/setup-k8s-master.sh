#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

# args:
# $1: The gateway IP to use
# $2: The gateway subnet mask
# $3: The default GW for the GW device

PUBLIC_IP=$1
PUBLIC_SUBNET_MASK=$2
GW_IP=$3
OVN_EXTERNAL=$4

if [ -n "$OVN_EXTERNAL" ]; then
    PUBLIC_IP=`ifconfig enp0s9 | grep 'inet addr' | cut -d: -f2 | awk '{print $1}'`
    PUBLIC_SUBNET_MASK=`ifconfig enp0s9 | grep 'inet addr' | cut -d: -f4`
    GW_IP=`grep 'option routers' /var/lib/dhcp/dhclient.enp0s9.leases | head -1 | sed -e 's/;//' | awk '{print $3}'`
fi

cat > setup_k8s_master_args.sh <<EOL
PUBLIC_IP=$1
PUBLIC_SUBNET_MASK=$2
GW_IP=$3
OVN_EXTERNAL=$4
EOL

# Install k8s

# Install an etcd cluster
sudo docker run --net=host -v /var/etcd/data:/var/etcd/data -d \
        gcr.io/google_containers/etcd:3.0.17 /usr/local/bin/etcd \
        --listen-peer-urls http://127.0.0.1:2380 \
        --advertise-client-urls=http://127.0.0.1:4001 \
        --listen-client-urls=http://0.0.0.0:4001 \
        --data-dir=/var/etcd/data

# Start k8s daemons
pushd k8s/server/kubernetes/server/bin
echo "Starting kube-apiserver ..."
nohup sudo ./kube-apiserver --service-cluster-ip-range=192.168.200.0/24 \
                            --address=0.0.0.0 --etcd-servers=http://127.0.0.1:4001 \
                            --v=2 2>&1 0<&- &>/dev/null &

# Wait till kube-apiserver starts up
while true; do
    ./kubectl get nodes
    if [ $? -eq 0 ]; then
        break
    fi
    echo "waiting for kube-apiserver to start...."
    sleep 1
done

echo "Starting kube-controller-manager ..."
nohup sudo ./kube-controller-manager --master=127.0.0.1:8080 --v=2 2>&1 0<&- &>/dev/null &

echo "Starting kube-scheduler ..."
nohup sudo ./kube-scheduler --master=127.0.0.1:8080 --v=2 2>&1 0<&- &>/dev/null &

popd

# Create a kubeconfig file.
cat << KUBECONFIG >> ~/kubeconfig.yaml
apiVersion: v1
clusters:
- cluster:
    server: http://localhost:8080
  name: default-cluster
- cluster:
    server: http://localhost:8080
  name: local-server
- cluster:
    server: http://localhost:8080
  name: ubuntu
contexts:
- context:
    cluster: ubuntu
    user: ubuntu
  name: ubuntu
current-context: ubuntu
kind: Config
preferences: {}
users:
- name: ubuntu
  user:
    password: p1NVMZqhOOOqkWQq
    username: admin
KUBECONFIG

source ~/setup_master_args.sh
nohup sudo ovnkube -kubeconfig $HOME/kubeconfig.yaml -net-controller -v=4 \
 -apiserver="http://$OVERLAY_IP:8080" \
 -logfile="/var/log/openvswitch/ovnkube.log" \
 -init-master="k8smaster" -cluster-subnet="192.168.0.0/16" \
 -ovn-north-db="tcp://$OVERLAY_IP:6631" \
 -ovn-south-db="tcp://$OVERLAY_IP:6632" 2>&1 &


# Setup some example yaml files
cat << APACHEPOD >> ~/apache-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: apachetwin
  labels:
    name: webserver
spec:
  containers:
  - name: apachetwin
    image: fedora/apache
APACHEPOD

cat << NGINXPOD >> ~/nginx-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: nginxtwin
  labels:
    name: webserver
spec:
  containers:
  - name: nginxtwin
    image: nginx
NGINXPOD

cat << APACHEEW >> ~/apache-e-w.yaml
apiVersion: v1
kind: Service
metadata:
  labels:
    name: apacheservice
    role: service
  name: apacheservice
spec:
  ports:
    - port: 8800
      targetPort: 80
      protocol: TCP
      name: tcp
  selector:
    name: webserver
APACHEEW

cat << APACHENS >> ~/apache-n-s.yaml
apiVersion: v1
kind: Service
metadata:
  labels:
    name: apacheexternal
    role: service
  name: apacheexternal
spec:
  ports:
    - port: 8800
      targetPort: 80
      protocol: TCP
      name: tcp
  selector:
    name: webserver
  type: NodePort
APACHENS

# Restore xtrace
$XTRACE
