#!/usr/bin/env bash
# The S1U robot x86 follows its node profile (node_profile.env). It runs as root in the host's namespaces, started
# by the node tuner (node_tuner.py) of the Addon s1u-robot: nobody logs in.
#   node_profile.sh install [node_profile.env]  the host's files: the kernel command line (GRUB), systemd's default
#       CPU affinity, the kubelet and container runtime drop-ins, kubelet's CPU manager and reserved memory, the boot
#       unit. Idempotent; every file it changes is first copied under /var/backups/rlark-s1u/<time>/.
#       It restarts nothing: what it wrote takes effect at the next boot.
#   node_profile.sh apply     what needs no restart: the governor, the reserved ports, the units' CPU shares, the
#       fair scheduler's period
#       (the boot unit runs this too).
#   node_profile.sh kubelet   before kubelet starts (its ExecStartPre): a CPU manager state of another policy, or of an
#       earlier kubelet configuration of the profile, removed.
#   node_profile.sh status    each rule of the profile against the running host, one line each: ok, DRIFT, or
#       REBOOT where only a restart of the host makes it hold, KUBELET where a restart of kubelet does; exit 1
#       unless all are ok.
#   node_profile.sh facts     what this host is, for the node's specification (node_spec.py), one value a line.
#   node_profile.sh busy host|kubelet [own]   why that restart must wait now (any component's mark under
#       /run/rlark/in-use, this Addon's own among them; the vendor driver runs, the motors are on), or nothing: exit 0
#       while it must wait. With "own", the robot's own reasons only (what this Addon's mark says).
#   node_profile.sh in-use [reason]   this Addon's mark that the host is in use (/run/rlark/in-use/s1u-robot, the
#       reason in it): written with a reason, removed without one.
#   node_profile.sh restart-kubelet   restarts kubelet (its containers keep running) unless the vendor's driver runs.
#   node_profile.sh uninstall the host's files as they were before the profile: what install created is removed, the kernel
#       command line goes back to its earliest copy under /var/backups/rlark-s1u. Like install it restarts
#       nothing: the host is as before from its next boot (the Addon's parameter profile=removed runs it).
#   node_profile.sh reboot    restarts the robot (the vendor's own restart: Orin and x86) unless the vendor's driver
#       runs or the motors are powered (exit 1 then). Whether either restart happened, and how often it is tried, is
#       the node tuner's (node_tuner.py).
# One profile for every S1U x86: nothing here names a robot.
set -Eeuo pipefail

PROFILE=/etc/rlark/s1u/node_profile.env
SELF=/usr/local/sbin/rlark-s1u-profile
IN_USE=/run/rlark/in-use/s1u-robot  # the directory: what each component of the host marks in use
STATE=/run/rlark/s1u
PORTS=/etc/sysctl.d/90-rlark-s1u.conf
SCHEDULER=/sys/kernel/debug/sched  # the fair scheduler's tunables (kernel 5.13 on)
BACKUP=/var/backups/rlark-s1u/$(date +%Y%m%d-%H%M%S)
CPUSET=/sys/fs/cgroup/cpuset
changed=()

cpus() {  # "0-1,6-7" as "0 1 6 7"
  local part; for part in ${1//,/ }; do seq -s ' ' "${part%-*}" "${part#*-}"; done | xargs
}

write() {  # write PATH MODE: stdin becomes PATH if it differs, the old file backed up first
  local path=$1 mode=$2 new; new=$(cat)
  [[ -f $path && "$(<"$path")" == "$new" ]] && return
  [[ -f $path ]] && install -D -m 600 "$path" "$BACKUP$path"
  install -D -m "$mode" /dev/stdin "$path" <<<"$new"
  changed+=("$path")
}

within() {  # within LIST ALLOWED: every CPU of LIST is one of ALLOWED
  local cpu
  for cpu in $(cpus "$1"); do [[ " $(cpus "$2") " == *" $cpu "* ]] || return 1; done
}

allowed() {  # the CPUs a thread may run on: allowed /proc/PID or /proc/PID/task/TID
  awk '/^Cpus_allowed_list:/ {print $2}' "$1/status" 2>/dev/null
}

install_profile() {
  # The profile given, else the one beside this script (the repository's), else the one already installed.
  local source=${1:-$(dirname "$(realpath "$0")")/node_profile.env}
  [[ -f $source ]] || source=$PROFILE
  mkdir -p "$BACKUP" "$STATE"
  write "$PROFILE" 644 < "$source"
  # Each body's cameras, for the robot's device layer and video (the one list both read).
  write "${PROFILE%/*}/bodies.json" 644 < "$(dirname "$source")/bodies.json"
  [[ $(realpath "$0") == "$SELF" ]] || write "$SELF" 755 < "$0"
  source "$PROFILE"
  local isolated="$S1U_VENDOR_REALTIME_CPUS,$S1U_SDK_CPUS"

  # The kernel command line: both isolated sets, and the SDK core's RCU callbacks elsewhere.
  local line current wanted
  line=$(grep '^GRUB_CMDLINE_LINUX_DEFAULT=' /etc/default/grub)
  current=${line#*=\"}; current=${current%\"}
  wanted="$(sed -E 's/(^| )(isolcpus|rcu_nocbs)=[^ ]*//g' <<<"$current" | xargs) isolcpus=$isolated rcu_nocbs=$S1U_SDK_CPUS"
  if [[ $current != "$wanted" ]]; then
    sed "s|^GRUB_CMDLINE_LINUX_DEFAULT=.*|GRUB_CMDLINE_LINUX_DEFAULT=\"$wanted\"|" /etc/default/grub \
      | write /etc/default/grub 644
    install -D -m 600 /boot/grub/grub.cfg "$BACKUP/boot/grub/grub.cfg"
    update-grub
    changed+=("/etc/default/grub: $wanted")
  fi

  # Everything the host starts (rc.local and robot_start.service with the vendor's processes, sshd, the desktop).
  write /etc/systemd/system.conf.d/rlark-s1u.conf 644 <<UNIT
# scripts/deploy/robot/node_profile.sh: S1U_HOST_CPUS. The vendor driver's realtime threads pin themselves to the
# isolated CPUs $S1U_VENDOR_REALTIME_CPUS as before.
[Manager]
CPUAffinity=$(cpus "$S1U_HOST_CPUS")
UNIT
  # Whose turn it is where the vendor's processes share CPUs with the host: the vendor's units and the system's
  # services ahead of login sessions and the desktop (apply sets the same on the running units).
  write /etc/systemd/system/user.slice.d/rlark-s1u-cpu.conf 644 <<UNIT
# scripts/deploy/robot/node_profile.sh: S1U_SESSION_CPU_SHARES.
[Slice]
CPUShares=$S1U_SESSION_CPU_SHARES
UNIT
  write /etc/systemd/system/system.slice.d/rlark-s1u-cpu.conf 644 <<UNIT
# scripts/deploy/robot/node_profile.sh: S1U_VENDOR_CPU_SHARES.
[Slice]
CPUShares=$S1U_VENDOR_CPU_SHARES
UNIT
  local vendor
  for vendor in $S1U_VENDOR_UNITS; do
    write /etc/systemd/system/$vendor.service.d/rlark-s1u-cpu.conf 644 <<UNIT
# scripts/deploy/robot/node_profile.sh: S1U_VENDOR_CPU_SHARES.
[Service]
CPUShares=$S1U_VENDOR_CPU_SHARES
UNIT
  done
  # k8s's daemons (kubelet and its container runtime), whose containers start on their CPUs. kubelet must read
  # /etc/kubernetes/kubelet.conf.d (below): where its unit does not say so (a robot enrolled before RLark's
  # installer did), its start line is repeated here with --config-dir.
  local unit start
  # systemd's view of kubelet's unit as it is on disk: a drop-in that removes itself at kubelet's start (an
  # uninstall's, this script's or Fleet's before it) is still loaded until a reload, and `systemctl cat` fails on it
  # (s1u-01 11-14, after the cutover's kubelet restart: install failed at every start of the tuner).
  systemctl daemon-reload
  for unit in $(kubernetes_units); do
    start=
    if [[ $unit == kubelet ]]; then
      # "-": kubelet starts whatever this step says (RLark's installer may have reset the node meanwhile).
      start="ExecStartPre=-$SELF kubelet"
      systemctl cat kubelet | awk '/^# \// {ours = /rlark-s1u-cpu.conf$/} !ours' | grep -q -- --config-dir \
        || start+=$'\nExecStart=\n'"$(systemctl cat kubelet | awk '/^# \// {ours = /rlark-s1u-cpu.conf$/} !ours && /^ExecStart=./ {line = $0}
            END {print line}') --config-dir=/etc/kubernetes/kubelet.conf.d"
    fi
    write /etc/systemd/system/$unit.service.d/rlark-s1u-cpu.conf 644 <<UNIT
# scripts/deploy/robot/node_profile.sh: S1U_KUBERNETES_CPUS.
[Service]
CPUAffinity=$(cpus "$S1U_KUBERNETES_CPUS")
$start
UNIT
  done
  # The Pods' CPUs, by kubelet's own CPU manager: S1U_SDK_CPUS, S1U_DEVICE_CPUS and S1U_KUBERNETES_CPUS are
  # the Pods' pool, everything else of the host is reserved and kept out of it. Within the pool the device layer
  # keeps itself to the first two by its own affinity; kubelet cannot be made to give it exactly those (it hands
  # out this CPU's single-thread cores first: device_pod.yaml). Their memory together: S1U_KUBERNETES_MEMORY_MIB.
  local total; total=$(awk '/^MemTotal:/ {print int($2 / 1024)}' /proc/meminfo)
  write /etc/kubernetes/kubelet.conf.d/50-rlark-s1u.conf 644 <<CONFIG
# scripts/deploy/robot/node_profile.sh, of the host's $total MiB and $(nproc --all) CPUs.
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cpuManagerPolicy: static
reservedSystemCPUs: "$(reserved)"
$(strict)
systemReserved:
  memory: $((total - S1U_KUBERNETES_MEMORY_MIB))Mi
# Stated whole: a kubelet given some thresholds drops its defaults for the others.
evictionHard:
  memory.available: ${S1U_EVICTION_MEMORY_MIB}Mi
  nodefs.available: 10%
  nodefs.inodesFree: 5%
  imagefs.available: 15%
  imagefs.inodesFree: 5%
imageGCHighThresholdPercent: $S1U_IMAGE_GC_HIGH_PERCENT
imageGCLowThresholdPercent: $S1U_IMAGE_GC_LOW_PERCENT
imageMaximumGCAge: $S1U_IMAGE_GC_MAXIMUM_AGE
CONFIG
  # The ports the robot's own services listen on are in the kernel's range for outgoing connections
  # (32768-60999): one of those holding the port keeps the service from starting (the Orin container on s1u-02-x86,
  # 11-13: "Address already in use" on :33336 four times in a row).
  write $PORTS 644 <<<"net.ipv4.ip_local_reserved_ports = $S1U_LOCAL_PORTS"
  # At boot, before k8s is up: the governor.
  write /etc/systemd/system/rlark-s1u-profile.service 644 <<UNIT
[Unit]
Description=s1u-robot: the robot x86 follows its node profile ($PROFILE)
Before=kubelet.service
# The fair scheduler's tunables are files of debugfs.
After=sys-kernel-debug.mount

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$SELF apply

[Install]
WantedBy=multi-user.target
UNIT
  # Every boot timed from the power button, milestone by milestone (boot_report.py).
  write /usr/local/sbin/rlark-s1u-boot-report 755 < "$(dirname "$(realpath "$0")")/boot_report.py"
  write /etc/systemd/system/rlark-s1u-boot-report.service 644 <<UNIT
[Unit]
Description=s1u-robot: how long this boot took, milestone by milestone ($STATE/boot_report.json)
DefaultDependencies=no
After=local-fs.target

[Service]
ExecStart=/usr/bin/python3 /usr/local/sbin/rlark-s1u-boot-report

[Install]
WantedBy=sysinit.target
UNIT
  systemctl daemon-reload
  systemctl enable --quiet rlark-s1u-profile.service rlark-s1u-boot-report.service
  rmdir "$BACKUP" 2>/dev/null || true
  if ((${#changed[@]})); then
    printf 'installed: %s\n' "${changed[@]}"
    [[ -d $BACKUP ]] && echo "backups: $BACKUP"
  fi
}

kubernetes_units() {  # kubelet and the container runtime it talks to, as this host names its unit
  local socket pid
  socket=$(tr '\0' '\n' < "/proc/$(systemctl show -p MainPID --value kubelet)/cmdline" \
    | sed -n 's|^--container-runtime-endpoint=unix://||p')
  # Where the endpoint is in kubelet's configuration file instead of its arguments.
  [[ -n $socket ]] || socket=$(sed -n 's|^containerRuntimeEndpoint: *"\{0,1\}unix://\([^"]*\)"\{0,1\}$|\1|p' /var/lib/kubelet/config.yaml)
  pid=$(ss -Hxlp | awk -v socket="$socket" '$5 == socket' | grep -o 'pid=[0-9]*' | head -1)
  echo "kubelet $(ps -o unit= -p "${pid#pid=}" | sed 's/\.service$//')"
}

strict() {
  # The static CPU manager's shared pool, which every Pod without exclusive CPUs runs on, is all CPUs but the
  # exclusive ones: the reserved CPUs too, unless strict-cpu-reservation takes them out (on by default from kubelet
  # 1.33; an unknown option stops an older kubelet). Measured on s1u-01-x86, kubelet 1.31 (11-12): such Pods got
  # 0-19 instead of S1U_KUBERNETES_CPUS.
  local minor
  minor=$("/proc/$(systemctl show -p MainPID --value kubelet)/exe" --version | sed -E 's/.*v1\.([0-9]+).*/\1/')
  ((minor < 33)) || printf 'cpuManagerPolicyOptions:\n  strict-cpu-reservation: "true"\n'
}

ranges() {  # "3-5,8-9" as the kernel prints a CPU list
  cpus "$1" | tr ' ' '\n' | sort -n | awk 'NR == 1 {a = b = $1; next} $1 == b + 1 {b = $1; next}
    {printf "%s%s", sep, (a == b ? a : a "-" b); sep = ","; a = b = $1} END {printf "%s%s\n", sep, (a == b ? a : a "-" b)}'
}

reserved() {  # every CPU of the host that no Pod may use, as a CPU list
  local cpu list=()
  for cpu in $(seq 0 $(($(nproc --all) - 1))); do
    within "$cpu" "$S1U_SDK_CPUS,$S1U_DEVICE_CPUS,$S1U_KUBERNETES_CPUS" || list+=("$cpu")
  done
  ranges "$(IFS=,; echo "${list[*]}")"
}

kubelet() {
  # kubelet refuses to start on the state file of another CPU manager policy, or of other reserved CPUs or policy
  # options than its configuration's now (a profile written since, e.g. for a newer kubelet): it then starts from
  # none and assigns the running containers again. Which configuration the state is of is kept beside it (kubelet
  # rewrites the state at every container, so its age says nothing); status reads it as what kubelet runs with.
  local state=/var/lib/kubelet/cpu_manager_state
  if ! grep -q '"policyName":"static"' $state 2>/dev/null || [[ $(configured) != "$(cat $state.profile 2>/dev/null)" ]]; then
    rm -f $state
  fi
  configured > $state.profile
}

configured() {
  sha256sum < /etc/kubernetes/kubelet.conf.d/50-rlark-s1u.conf
}

uninstall() {
  # Everything install wrote is the profile's own and is removed (a copy kept under $BACKUP), but the kernel command
  # line, which goes back to the earliest copy write kept of it (none: install never changed it).
  mkdir -p "$BACKUP"
  systemctl disable --quiet rlark-s1u-profile.service rlark-s1u-boot-report.service 2>/dev/null || true
  rm -f "$IN_USE"
  rmdir /var/lib/rlark/s1u 2>/dev/null || true  # the Orin's container emptied it
  local path original
  for path in "$PROFILE" "${PROFILE%/*}/bodies.json" /etc/systemd/system.conf.d/rlark-s1u.conf \
      /etc/systemd/system/*.service.d/rlark-s1u-cpu.conf /etc/systemd/system/*.slice.d/rlark-s1u-cpu.conf \
      /etc/kubernetes/kubelet.conf.d/50-rlark-s1u.conf \
      $PORTS /etc/systemd/system/rlark-s1u-profile.service /etc/systemd/system/rlark-s1u-boot-report.service \
      /usr/local/sbin/rlark-s1u-boot-report "$SELF" /var/lib/kubelet/cpu_manager_state.profile; do
    [[ -f $path ]] || continue
    install -D -m 600 "$path" "$BACKUP$path"
    rm "$path"
    changed+=("$path")
  done
  rmdir --ignore-fail-on-non-empty /etc/rlark/s1u /etc/systemd/system/*.service.d /etc/systemd/system/*.slice.d 2>/dev/null || true
  rm -rf /var/lib/rlark/s1u
  original=$(ls /var/backups/rlark-s1u/*/etc/default/grub 2>/dev/null | head -1)
  if [[ -n $original ]] && ! cmp -s "$original" /etc/default/grub; then
    install -D -m 600 /etc/default/grub "$BACKUP/etc/default/grub"
    install -m 644 "$original" /etc/default/grub
    update-grub
    changed+=("/etc/default/grub: $(grep '^GRUB_CMDLINE_LINUX_DEFAULT=' /etc/default/grub) (from $original)")
  fi
  # kubelet refuses to start without the static policy on the state the static policy left. For its next start
  # only: the state goes, and this line with it.
  if [[ -f /var/lib/kubelet/cpu_manager_state ]] && ! grep -q '"policyName":"none"' /var/lib/kubelet/cpu_manager_state; then
    install -D -m 644 /dev/stdin /etc/systemd/system/kubelet.service.d/rlark-s1u-cpu.conf <<UNIT
# Left by rlark-s1u-profile uninstall for kubelet's next start, which removes it.
[Service]
ExecStartPre=-/bin/rm -f /var/lib/kubelet/cpu_manager_state /etc/systemd/system/kubelet.service.d/rlark-s1u-cpu.conf
UNIT
  fi
  systemctl daemon-reload
  rmdir "$BACKUP" 2>/dev/null || true
  ((${#changed[@]} == 0)) || printf 'removed: %s\n' "${changed[@]}"
}

facts() {
  # What this host is, for the node's specification (node_spec.py): "<scope> <name>=<value>" lines. model: the same
  # on every S1U; site: on every robot of a site; body: of a body type; record: one node's own, in no fingerprint.
  source "$PROFILE"
  local runtime pid config kubelet body=$S1U_VENDOR_BODY_PARAMETERS
  runtime=$(kubernetes_units | cut -d' ' -f2)
  pid=$(systemctl show -p MainPID --value "$runtime")
  kubelet=$(systemctl show -p MainPID --value kubelet)
  config=$(tr '\0' '\n' < "/proc/$pid/cmdline" | grep -A1 -x -- --config | tail -1)
  config=${config:-/etc/containerd/config.toml}
  echo "model os=$(. /etc/os-release; echo "$ID $VERSION_ID")"
  echo "model kernel=$(uname -r)"
  echo "model machine=$(cat /sys/class/dmi/id/product_name), $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs), $(nproc --all) CPUs, $(awk '/^MemTotal:/ {print int($2 / 1048576 + 0.5)}' /proc/meminfo) GiB"
  echo "model vendor software=$(sha256sum < "$S1U_VENDOR_VERSION" | cut -c1-16)"
  echo "model container runtime version=$("/proc/$pid/exe" --version | awk '{print $3}' | sed 's/^v//')"
  echo "model container runtime=$(readlink "/proc/$pid/exe") $(tr '\0' ' ' < "/proc/$pid/cmdline" | cut -d' ' -f2- | xargs)"
  echo "model container runtime configuration=$(sha256sum < "$config" | cut -c1-16)"
  echo "model kubelet=$("/proc/$kubelet/exe" --version | awk '{print $2}') $(readlink "/proc/$kubelet/exe")"
  # Its arguments but the two that name the node.
  echo "model kubelet arguments=$(tr '\0' '\n' < "/proc/$kubelet/cmdline" | tail -n +2 | grep -v -e '^--node-ip=' -e '^--hostname-override=' | xargs)"
  echo "model kernel isolation=$(tr ' ' '\n' < /proc/cmdline | grep -E '^(isolcpus|rcu_nocbs)=' | xargs)"
  echo "model node profile=$(cat "$PROFILE" /etc/kubernetes/kubelet.conf.d/50-rlark-s1u.conf | sha256sum | cut -c1-16)"
  # A credential is a value: a key of containerd's registry configuration given one (its empty section header
  # [...registry.auths] is none; 11-14 that header alone read as "present" on both robots), or kubelet's own file.
  echo "model registry credentials on the node=$(
    { grep -qiE '^\s*(auth|username|password|identitytoken)\s*=' "$config" || [[ -e /var/lib/kubelet/config.json ]]; } \
      && echo present || echo none)"
  echo "site trusted registries=$(ls "$(dirname "$config")/certs.d" 2>/dev/null | xargs)"
  # The file kubelet gives its Pods' resolver from: its configuration file, or a later fragment of its directory
  # (RLark's installer sets it there).
  echo "site name servers=$(awk '/^nameserver/ {print $2}' "$(cat /var/lib/kubelet/config.yaml /etc/kubernetes/kubelet.conf.d/*.conf \
    | sed -n 's/^resolvConf: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' | tail -1)" | xargs)"
  echo "site image registry=$S1U_IMAGE_REGISTRY"
  echo "site gateway=$S1U_SITE_GATEWAY"
  echo "site time source=${S1U_SITE_TIME_SOURCE:-}"  # none declared: no variable at all (set -u)
  echo "body grippers=$(sed -n '/^gripper_version:/,/^[^ ]/ s/^  *\(left\|right\): */\1 /p' "$body" | xargs)"
  echo "body motors=$(sed -n 's/^motor_version: *\([^ ]*\).*/\1/p' "$body")"
  echo "record firmware=$(cat /sys/class/dmi/id/bios_version)"
  echo "record body serial=$(sed -n 's/^serial_number: *//p' "$body")"
  echo "record body parameters=$(sha256sum < "$body" | cut -c1-16)"
}

shares() {  # each unit whose CPU shares the profile sets, with them
  echo "user.slice $S1U_SESSION_CPU_SHARES"
  echo "system.slice $S1U_VENDOR_CPU_SHARES"
  local unit; for unit in $S1U_VENDOR_UNITS; do echo "$unit.service $S1U_VENDOR_CPU_SHARES"; done
}

shares_of() {  # the file of a unit's CPU shares (cgroup v1, as the robot's kubelet runs)
  echo "/sys/fs/cgroup/cpu$(systemctl show -p ControlGroup --value "$1")/cpu.shares"
}

in_use() {
  # RLark replaces its own programs on a host (station-agent-host: /opt/rlark/bin/station-gateway, the site tunnel)
  # only while /run/rlark/in-use is empty. The platform does not know when a robot is in use: the vendor's driver and motors are this Addon's to say, a Station attached its device layer's.
  if [[ -n ${1:-} ]]; then
    mkdir -p "${IN_USE%/*}"
    echo "$1" > "$IN_USE"
  else
    rm -f "$IN_USE"
  fi
}

busy() {  # busy host|kubelet [own]: says why that restart must wait and returns 0, or says nothing and returns 1
  source "$PROFILE"
  exec 2> /dev/null  # what it says is the reason, and only that: a command's complaint is not one
  # The host is in use while any component marks it (/run/rlark/in-use/<its name>: a Station attached to the robot's
  # device layer, this Addon for the vendor driver), whoever asks: one signal for every disruption of the robot.
  local mark
  for mark in "${IN_USE%/*}"/*; do
    [[ ${2:-} != own && -f $mark ]] || continue
    echo "${mark##*/}: $(head -c 200 "$mark")"; return 0
  done
  if driver_runs; then echo "$S1U_VENDOR_DRIVER runs"; return 0; fi
  [[ $1 == host ]] || return 1
  set +u; source /opt/ros/noetic/setup.bash; set -u  # ROS's setup reads variables it never set
  export ROS_MASTER_URI=http://$S1U_ROS_IP_ADDRESS:11311
  # The vendor's system monitor publishes a status code of module 12 while the motors are on (without the monitor
  # nothing powered them): the vendor's restart would cut their power.
  if timeout 5 rostopic echo -n1 /astribot_error_code/multi_error_code 2>/dev/null | grep -Eq '(^|[",])12[0-9]{6}'; then
    echo "its motors are powered"; return 0
  fi
  return 1
}

apply() {
  source "$PROFILE"
  local governor
  for governor in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
    echo "$S1U_CPU_GOVERNOR" > "$governor"
  done
  sysctl -q -w net.ipv4.ip_local_reserved_ports="$S1U_LOCAL_PORTS"
  # Everything but the robot's own network leaves through the Orin: the default route, in NetworkManager's
  # connection for the next boot and on the running host (s1u_02 11-14: a connection without a gateway, the host
  # reached the Hubs by RLark's routes and nothing else of the site).
  local device
  device=$(ip -4 -o addr show to "$S1U_ROS_IP_ADDRESS" | awk '{print $2}')
  if [[ -n $device && -z $(ip -4 route show default via "$S1U_ORIN_IP_ADDRESS" dev "$device") ]]; then
    nmcli connection modify "$(nmcli -g GENERAL.CONNECTION device show "$device")" ipv4.gateway "$S1U_ORIN_IP_ADDRESS"
    ip route replace default via "$S1U_ORIN_IP_ADDRESS" dev "$device" proto static metric 100
    echo "default route via $S1U_ORIN_IP_ADDRESS on $device"
  fi
  local unit shares
  while read -r unit shares; do
    [[ $(cat "$(shares_of "$unit")" 2>/dev/null) == "$shares" ]] || systemctl set-property --runtime "$unit" CPUShares="$shares"
  done < <(shares)
  # What Fleet left on the host before RLark (S1U_RETIRED_*): taken off once, reported by status while any is left.
  local unit path
  for unit in $S1U_RETIRED_UNITS; do
    if [[ -e /etc/systemd/system/$unit.service ]]; then
      systemctl disable --now --quiet "$unit.service"
      rm -rf "/etc/systemd/system/$unit.service" "/etc/systemd/system/$unit.service.d"
      systemctl daemon-reload
      echo "retired $unit.service"
    fi
    if [[ $(systemctl show -p ActiveState --value "$unit.service") == failed ]]; then
      systemctl reset-failed "$unit.service"
    fi
  done
  for path in $S1U_RETIRED_PATHS; do
    if [[ -e $path ]]; then
      rm -rf "$path"
      echo "removed $path"
    fi
  done
  for path in $S1U_RETIRED_DIRECTORIES; do
    if [[ -d $path ]] && ! mountpoint -q "$path" && [[ -z $(ls -A "$path") ]]; then
      rmdir "$path"
      echo "removed $path"
    fi
  done
  echo "$S1U_SCHEDULER_PERIOD_NS" > $SCHEDULER/latency_ns
  echo "$S1U_SCHEDULER_SLICE_NS" > $SCHEDULER/min_granularity_ns
  echo "$S1U_SCHEDULER_WAKEUP_NS" > $SCHEDULER/wakeup_granularity_ns
  echo HRTICK > $SCHEDULER/features
}

driver_runs() {
  pgrep -x "$S1U_VENDOR_DRIVER" > /dev/null
}

retired() {  # what of the profile's retired units, paths and directories is still on the host, one a line
  local unit path
  for unit in $S1U_RETIRED_UNITS; do
    [[ ! -e /etc/systemd/system/$unit.service && $(systemctl show -p ActiveState --value "$unit.service") != failed ]] \
      || echo "$unit.service"
  done
  for path in $S1U_RETIRED_PATHS $S1U_RETIRED_DIRECTORIES; do
    [[ ! -e $path ]] || echo "$path$(mountpoint -q "$path" && echo " (mounted)")"
  done
}

status() {
  source "$PROFILE"
  local drift=0 unit pid task list
  rule() {  # rule NAME ACTUAL EXPECTED [REBOOT|KUBELET]: what restart makes it hold, where only one does
    if [[ $2 == "$3" ]]; then echo "ok     $1: ${2:-none}"; else echo "${4:-DRIFT}  $1: ${2:-none} (profile: ${3:-none})"; drift=1; fi
  }
  rule "isolated CPUs (kernel command line)" "$(cat /sys/devices/system/cpu/isolated)" \
    "$(ranges "$S1U_VENDOR_REALTIME_CPUS,$S1U_SDK_CPUS")" REBOOT
  rule "systemd's default affinity (the vendor's processes, the host)" "$(allowed /proc/1)" "$S1U_HOST_CPUS" REBOOT
  rule "governor of every CPU" \
    "$(cat /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor | sort -u | xargs)" "$S1U_CPU_GOVERNOR"
  rule "route to the image registry ${S1U_IMAGE_REGISTRY%:*} (the uplink through the Orin, not the tunnel)" \
    "$(ip -o route get "${S1U_IMAGE_REGISTRY%:*}" | grep -o 'via [0-9.]*')" "via $S1U_ORIN_IP_ADDRESS"
  # The uplink: this host holds its address on the robot's network and no other, everything else leaves through
  # the Orin, which forwards it under its own address (s1u-orin-uplink.service there, kept by orin.py), whatever
  # address and uplink the Orin has. (Until 11-14 each x86 held a hand-picked address of the site's network that
  # the Orin answered ARP for; s1u_02's Orin was leased that very address and the x86 was gone from the site.)
  local device
  device=$(ip -4 -o addr show to "$S1U_ROS_IP_ADDRESS" | awk '{print $2}')
  rule "addresses on the robot's interface $device" "$(ip -4 -o addr show dev "$device" | awk '{print $4}' | cut -d/ -f1 | xargs)" \
    "$S1U_ROS_IP_ADDRESS"
  rule "default route" "$(ip -4 route show default | grep -o 'via [0-9.]*' | sort -u | xargs)" "via $S1U_ORIN_IP_ADDRESS"
  rule "the site answers this host's own address through the Orin (${S1U_IMAGE_REGISTRY%:*})" \
    "$(ping -c 1 -W 2 -I "$S1U_ROS_IP_ADDRESS" "${S1U_IMAGE_REGISTRY%:*}" > /dev/null 2>&1 && echo yes || echo no)" yes
  # The Orin's uplink addresses as its container last read them (orin.py): the x86 holding one of them answers
  # for the Orin or is hidden by it (s1u_02 11-13).
  if [[ -f $STATE/orin_addresses ]]; then
    rule "addresses of the Orin's uplink that this host holds too" \
      "$(comm -12 <(tr ' ' '\n' < $STATE/orin_addresses | sort) <(hostname -I | tr ' ' '\n' | sort) | xargs)" ""
    # All the uplink asks of a site: an address for the Orin that is not of the robot's own network (the Orin
    # could not tell the site from the robot), and a way from there to the Hubs, which the tunnel itself shows.
    rule "addresses of the Orin's uplink inside the robot's own network ${S1U_ROS_IP_ADDRESS%.*}.0/24" \
      "$(tr ' ' '\n' < $STATE/orin_addresses | grep "^${S1U_ROS_IP_ADDRESS%.*}\." | xargs)" ""
  else
    echo "note   addresses of the Orin's uplink: not read yet (the Orin's container)"
  fi
  rule "CPU shares (login sessions, system services, the vendor's units)" \
    "$(while read -r unit shares; do cat "$(shares_of "$unit")"; done < <(shares) | xargs)" \
    "$(shares | cut -d' ' -f2 | xargs)"
  rule "fair scheduler: period, slice, wakeup lead (ns), precise slices" \
    "$(cat $SCHEDULER/latency_ns) $(cat $SCHEDULER/min_granularity_ns) $(cat $SCHEDULER/wakeup_granularity_ns) $(tr ' ' '\n' < $SCHEDULER/features | grep -x -e HRTICK -e NO_HRTICK)" \
    "$S1U_SCHEDULER_PERIOD_NS $S1U_SCHEDULER_SLICE_NS $S1U_SCHEDULER_WAKEUP_NS HRTICK"
  rule "local ports kept from outgoing connections" "$(sysctl -n net.ipv4.ip_local_reserved_ports)" "$S1U_LOCAL_PORTS"
  rule "what Fleet left on the host before RLark" "$(retired | xargs)" ""
  # The vendor's units: every thread on the host's CPUs or, pinned there by the driver itself, the isolated ones.
  for unit in $S1U_VENDOR_UNITS; do
    # Each thread on all of the host's CPUs, or pinned by the vendor driver within its realtime ones. A process
    # started before the profile changed keeps the CPUs it was started with (s1u-02-x86 11-13: systemd itself had
    # the wider set after a re-execution, the vendor's 57 processes still 0-1,6-7), until the host restarts.
    rule "$unit.service threads not on $S1U_HOST_CPUS or within $S1U_VENDOR_REALTIME_CPUS" "$(
      for pid in $(cat "/sys/fs/cgroup/systemd$(systemctl show -p ControlGroup --value $unit.service)/cgroup.procs"); do
        for task in /proc/$pid/task/*; do
          list=$(allowed "$task")
          [[ -z $list || $list == "$(ranges "$S1U_HOST_CPUS")" ]] || within "$list" "$S1U_VENDOR_REALTIME_CPUS" \
            || echo "$(<"$task/comm"):$list"
        done
      done | sort -u | xargs | cut -c1-300)" "" REBOOT
  done
  for unit in $(kubernetes_units); do
    rule "$unit's affinity" "$(allowed "/proc/$(systemctl show -p MainPID --value $unit)")" "$S1U_KUBERNETES_CPUS" \
      "$([[ $unit == kubelet ]] && echo KUBELET || echo REBOOT)"
  done
  # kubelet reads its configuration when it starts: one written since holds only after a restart.
  rule "kubelet's CPU manager" "$(grep -o '"policyName":"[a-z]*"' /var/lib/kubelet/cpu_manager_state | cut -d'"' -f4), $(
    [[ $(configured) == "$(cat /var/lib/kubelet/cpu_manager_state.profile 2>/dev/null)" ]] \
      && echo "started with its configuration" || echo "started before its configuration")" \
    "static, started with its configuration" KUBELET
  rule "Pods' memory limit (MiB)" \
    "$(($(cat /sys/fs/cgroup/memory/kubepods.slice/memory.limit_in_bytes) / 1048576))" "$S1U_KUBERNETES_MEMORY_MIB" KUBELET
  # The SDK core: the device layer's command thread alone, at SCHED_FIFO. The kernel puts nothing else there (isolcpus), but
  # the core is in the Pods' pool, so another Pod's thread is allowed there and could be put there by its own
  # program: said as a note, with who is allowed, not a rule kubelet can be made to hold (device_pod.yaml).
  # Room on the disk, before kubelet removes images and evicts Pods for it: each filesystem of kubelet's state and
  # its container runtime's.
  local target free
  while read -r target free; do
    if ((free >= S1U_DISK_FREE_PERCENT)); then
      echo "ok     free space on $target: $free %"
    else
      echo "DRIFT  free space on $target: $free % (profile: at least $S1U_DISK_FREE_PERCENT %)"; drift=1
    fi
  done < <(df --output=target,pcent /var/lib /var/lib/kubelet | tail -n +2 | sort -u | awk '{print $1, 100 - $2}')
  echo "note   CPUs $S1U_SDK_CPUS (the SDK core) are in the Pods' pool: kept free by the kernel's isolation and the device layer's own affinity, not by kubelet"
  # Not a rule (a kubelet before 1.33 cannot hold it): where the Pods without exclusive CPUs may run.
  echo "note   the shared pool of Pods without exclusive CPUs: $(grep -o '"defaultCpuSet":"[^"]*"' \
    /var/lib/kubelet/cpu_manager_state | cut -d'"' -f4) (profile: $S1U_KUBERNETES_CPUS, kubelet $([[ -n $(strict) ]] \
    && echo "keeps the reserved CPUs out" || echo "before 1.33 lets them use the reserved CPUs too"))"
  # Not a rule either (a robot cut off from the site keeps its device layer): the clock against the site's time.
  echo "note   clock against the site's time source ${S1U_SITE_TIME_SOURCE:-(none in the profile)}: $(cat "$STATE/clock.json" 2>/dev/null || echo not measured)"
  mkdir -p "$STATE"
  return $drift
}

case "${1:-}" in
  install) install_profile "${2:-}" ;;
  apply) apply ;;
  kubelet) kubelet ;;
  status) status ;;
  uninstall) uninstall ;;
  facts) facts ;;
  busy) busy "${2:?host or kubelet}" "${3:-}" ;;
  in-use) in_use "${2:-}" ;;
  restart-kubelet)
    busy kubelet && exit 1
    echo "restarting kubelet for its node profile"
    systemctl restart kubelet ;;
  reboot)
    busy host && exit 1
    # The whole robot, as the vendor control centre's 重启 does (motors off, the Orin, then this x86): after a
    # restart of the x86 alone the Orin's nodes (the cameras) are unknown to the new ROS master (11-12). Without
    # the vendor's system monitor (a box whose vendor software is not up), the x86 alone.
    echo "restarting the robot for its node profile"
    # The vendor's service answers at once and checks nothing: whether the restart happened is the caller's to see
    # (node_tuner.py: the boot's identifier changes).
    if ! timeout 10 rosservice call /astribot_system_monitor/reboot_command "data: true"; then
      echo "the vendor's restart service did not answer: restarting the x86 alone"
      systemctl reboot
    fi ;;
  *) sed -n '2,30p' "$0" >&2; exit 2 ;;
esac
