#!/bin/sh
# Host tuning of slc-vaultaire-01 (docs/DEPLOY.md "Host tuning", 2026-10-21),
# run once at boot by vaultaire-host-tuning.service. Idempotent.
#   - CPU: the `performance` governor (amd-pstate-epp: EPP performance — the
#     cores reach full clock at once instead of ramping from ~58 %).
#   - NIC: 4096-descriptor rings (were 512 of 8192: rx_missed_errors 1,156,
#     rx_dropped 476k in 11 days); resizing resets the link for ~1 s, so it
#     is done only when the ring is not already 4096.
#   - IRQs: queue TxRx-<n> pinned to CPU n (irqbalance is off; with every
#     queue allowed on every CPU, CPU 16 took 110 M of the NIC's interrupts,
#     most CPUs 2–27 M).
set -u
IF=${VAULTAIRE_NIC:-enp1s0f0}
RING=${VAULTAIRE_NIC_RING:-4096}

for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
	echo performance >"$g" || echo "governor: $g refused" >&2
done

cur=$(ethtool -g "$IF" | awk '/^Current/{c=1} c && /^RX:/{print $2; exit}')
if [ "$cur" != "$RING" ]; then
	ethtool -G "$IF" rx "$RING" tx "$RING" || echo "ring: ethtool -G refused" >&2
fi

ncpu=$(nproc)
grep "$IF-TxRx-" /proc/interrupts | while read -r line; do
	irq=${line%%:*}
	irq=$(echo "$irq" | tr -d ' ')
	q=$(echo "$line" | grep -oE "TxRx-[0-9]+" | cut -d- -f2)
	echo $((q % ncpu)) >"/proc/irq/$irq/smp_affinity_list" || echo "irq $irq: refused" >&2
done
