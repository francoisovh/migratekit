FROM fedora:41 AS build
RUN dnf install -y golang libnbd-devel
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /migratekit main.go

FROM fedora:41
ADD https://fedorapeople.org/groups/virt/virtio-win/virtio-win.repo /etc/yum.repos.d/virtio-win.repo
RUN \
  dnf install --refresh -y nbdkit nbdkit-vddk-plugin libnbd virt-v2v virtio-win && \
  dnf clean all && \
  rm -rf /var/cache/dnf
COPY --from=build /migratekit /usr/local/bin/migratekit
ADD vmware-vix-disklib /usr/lib64/vmware-vix-disklib
ENTRYPOINT ["/usr/local/bin/migratekit"]
