//go:build linux
// +build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	// A line of self/mountinfo has the following structure:
	// 36  35  98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
	// (0) (1) (2)   (3)   (4)      (5)      (6)   (7) (8)    (9)           (10)
	//
	// (0) mount ID: unique identifier of the mount (may be reused after umount).
	//mountinfoMountID = 0
	// (1) parent ID: ID of parent (or of self for the top of the mount tree).
	//mountinfoParentID = 1
	// (2) major:minor: value of st_dev for files on filesystem.
	//mountinfoMajorMinor = 2
	// (3) root: root of the mount within the filesystem.
	//mountinfoRoot = 3
	// (4) mount point: mount point relative to the process's root.
	mountinfoMountPoint = 4
	// (5) mount options: per mount options.
	mountinfoMountOpts = 5
	// (6) optional fields: zero or more fields terminated by "-".
	mountinfoOptionalFields = 6
	// (7) separator between optional fields.
	//mountinfoSeparator = 7
	// (8) filesystem type: name of filesystem of the form.
	mountinfoFsType = 8
	// (9) mount source: filesystem specific information or "none".
	mountinfoMountSource = 9
	// (10) super options: per super block options.
	//mountinfoSuperOptions = 10
)

// Stat returns the mountpoint's stat information.
func (m *Mount) Stat() unix.Statfs_t {
	return m.Metadata.(unix.Statfs_t)
}

func mounts() ([]Mount, []string, error) {
	var warnings []string

	filename := "/proc/self/mountinfo"
	lines, err := readLines(filename)
	if err != nil {
		return nil, nil, err
	}

	ret := make([]Mount, 0, len(lines))
	for _, line := range lines {
		nb, fields := parseMountInfoLine(line)
		if nb == 0 {
			continue
		}

		// if the number of fields does not match the structure of mountinfo,
		// emit a warning and ignore the line.
		if nb < 10 || nb > 11 {
			warnings = append(warnings, fmt.Sprintf("found invalid mountinfo line: %s", line))
			continue
		}

		// blockDeviceID := fields[mountinfoMountID]
		mountPoint := fields[mountinfoMountPoint]
		mountOpts := fields[mountinfoMountOpts]
		fstype := fields[mountinfoFsType]
		device := fields[mountinfoMountSource]

		var stat unix.Statfs_t
		err := unix.Statfs(mountPoint, &stat)
		if err != nil {
			if err != os.ErrPermission {
				warnings = append(warnings, fmt.Sprintf("%s: %s", mountPoint, err))
				continue
			}

			stat = unix.Statfs_t{}
		}

		d := Mount{
			Device:     device,
			Mountpoint: mountPoint,
			Fstype:     fstype,
			Type:       fsTypeMap[int64(stat.Type)], //nolint:unconvert
			Opts:       mountOpts,
			Metadata:   stat,
			Total:      (uint64(stat.Blocks) * uint64(stat.Bsize)),                      //nolint:unconvert
			Free:       (uint64(stat.Bavail) * uint64(stat.Bsize)),                      //nolint:unconvert
			Used:       (uint64(stat.Blocks) - uint64(stat.Bfree)) * uint64(stat.Bsize), //nolint:unconvert
			Inodes:     stat.Files,
			InodesFree: stat.Ffree,
			InodesUsed: stat.Files - stat.Ffree,
			Blocks:     uint64(stat.Blocks), //nolint:unconvert
			BlockSize:  uint64(stat.Bsize),
		}
		d.DeviceType = deviceType(d)

		// resolve /dev/mapper/* device names
		if strings.HasPrefix(d.Device, "/dev/mapper/") {
			re := regexp.MustCompile(`^\/dev\/mapper\/(.*)-(.*)`)
			match := re.FindAllStringSubmatch(d.Device, -1)
			if len(match) > 0 && len(match[0]) == 3 {
				d.Device = filepath.Join("/dev", match[0][1], match[0][2])
			}
		}

		ret = append(ret, d)
	}

	return ret, warnings, nil
}

// parseMountInfoLine parses a line of /proc/self/mountinfo and returns the
// amount of parsed fields and their values.
func parseMountInfoLine(line string) (int, [11]string) {
	var fields [11]string

	if len(line) == 0 || line[0] == '#' {
		// ignore comments and empty lines
		return 0, fields
	}

	// Handle simple cases without the separator
	if !strings.Contains(line, " - ") {
		parts := strings.Fields(line)
		if len(parts) == 0 {
			return 0, fields
		}
		if len(parts) <= 6 {
			for i := 0; i < len(parts); i++ {
				fields[i] = parts[i]
			}
			return len(parts), fields
		}
		// If more than 6 fields, join the rest
		for i := 0; i < 6; i++ {
			fields[i] = parts[i]
		}
		fields[6] = strings.Join(parts[6:], " ")
		return 6, fields
	}

	// Split the line into parts before and after the separator "-"
	parts := strings.SplitN(line, " - ", 2)
	if len(parts) != 2 {
		return 0, fields
	}

	// Handle the first part (before the separator)
	firstParts := strings.Fields(parts[0])
	if len(firstParts) < 6 {
		return 0, fields
	}

	// Fill in the first 6 fields
	for i := 0; i < 6 && i < len(firstParts); i++ {
		if i == mountinfoMountPoint {
			fields[i] = unescapeFstab(firstParts[i])
		} else {
			fields[i] = firstParts[i]
		}
	}

	// Handle optional fields (field 6)
	if len(firstParts) > 6 {
		fields[mountinfoOptionalFields] = strings.Join(firstParts[6:], " ")
	}

	// Add the separator
	fields[7] = "-"

	// Handle the second part (after the separator)
	secondParts := strings.SplitN(strings.TrimSpace(parts[1]), " ", 3)
	if len(secondParts) < 2 {
		return 0, fields
	}

	// Fill in filesystem type and mount source
	fields[mountinfoFsType] = unescapeFstab(secondParts[0])
	fields[mountinfoMountSource] = unescapeFstab(secondParts[1])

	// Add super options if present
	if len(secondParts) > 2 {
		fields[10] = secondParts[2]
	}

	// Count non-empty fields
	count := len(fields)
	for i := len(fields) - 1; i >= 0; i-- {
		if fields[i] != "" {
			break
		}
		count--
	}

	return count, fields
}
