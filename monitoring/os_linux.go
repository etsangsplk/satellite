/*
Copyright 2017 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package monitoring

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/gravitational/satellite/agent/health"
	pb "github.com/gravitational/satellite/agent/proto/agentpb"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

// NewOSChecker returns a new checker to verify OS distribution
// against the list of supported releases.
//
// The specified releases are allowed to relax a version constraint
// by providing a version prefix in VersionID field to match all of
// the minor versions:
//
// So, for example:
// NewOSChecker(OSRelease{Name: "Ubuntu", VersionID: "16"})
//
// will match all 16.x ubuntu distribution releases.
func NewOSChecker(releases ...OSRelease) health.Checker {
	return &osReleaseChecker{
		Releases:   releases,
		getRelease: GetRealOSRelease,
	}
}

// GetRealOSRelease deteremins the OS distribution release information
func GetRealOSRelease() (info *OSRelease, err error) {
	return getOSReleaseFromFiles(releases, versions)
}

// osReleaseChecker validates host OS based on
// https://www.freedesktop.org/software/systemd/man/os-release.html
type osReleaseChecker struct {
	// Releases lists all supported releases
	Releases   []OSRelease
	getRelease osReleaseGetter
}

// Name returns name of the checker
func (c *osReleaseChecker) Name() string {
	return osCheckerID
}

// Check checks current OS and release is within supported list
func (c *osReleaseChecker) Check(ctx context.Context, reporter health.Reporter) {
	info, err := c.getRelease()
	if err != nil {
		reporter.Add(NewProbeFromErr(osCheckerID,
			"failed to query OS version",
			trace.Wrap(err)))
		return
	}

	for _, release := range c.Releases {
		if versionsMatch(release, *info) {
			reporter.Add(&pb.Probe{
				Checker: osCheckerID,
				Status:  pb.Probe_Running,
			})
			return
		}
	}

	reporter.Add(&pb.Probe{
		Checker: osCheckerID,
		Detail:  fmt.Sprintf("%s %s is not supported", info.ID, info.VersionID),
		Status:  pb.Probe_Failed,
	})
}

// OSRelease is used to represent a certain OS release
// based on https://www.freedesktop.org/software/systemd/man/os-release.html
type OSRelease struct {
	// ID identifies the distributor: ubuntu, redhat/centos, etc.
	ID string
	// VersionID is the release version i.e. 16.04 for Ubuntu
	VersionID string
}

// Name returns a name/version for this OS info, e.g. "centos 7.1"
func (r OSRelease) Name() string {
	return fmt.Sprintf("%v %v", r.ID, r.VersionID)
}

func getOSReleaseFromFiles(releases, versions []string) (info *OSRelease, err error) {
	release, err := openFirst(releases...)
	if err != nil {
		log.Warnf("Failed to read any release file: %v.", err)
		return nil, trace.NotFound("no release version file found")
	}

	version, err := openFirst(versions...)
	if err != nil {
		log.Warnf("Failed to read any release version file: %v.", err)
		// fallthrough
	}

	return getOSRelease(releaseFiles{
		release:    release,
		version:    version,
		lsbRelease: lsbRelease,
	})
}

func getOSRelease(files releaseFiles) (info *OSRelease, err error) {
	// Try lsb_release first
	if files.lsbRelease != nil {
		info, err = files.lsbRelease()
		if err == nil {
			return info, nil
		}
	}

	info, err = files.releaseInfo()
	if err != nil {
		return nil, trace.Wrap(err, "failed to determine release version")
	}

	if info == nil {
		// At this point, we cannot determine release information
		return nil, trace.NotFound("no release information found")
	}

	files.updateInfo(info)

	return info, nil
}

// versionsMatch tests if test is equivalent to info
func versionsMatch(test, info OSRelease) bool {
	if strings.ToLower(test.ID) != strings.ToLower(info.ID) {
		return false
	}

	// Versions are matched as prefixes, e.g. if a required version is 7.2 then
	// it matches 7.2, 7.2.1, 7.2.2, etc.
	if !strings.HasPrefix(info.VersionID, test.VersionID) {
		return false
	}

	return true
}

const (
	// osCheckerID identifies this checker
	osCheckerID = "os-checker"

	// fieldReleaseID specifies the name of the field with OS distribution ID
	fieldReleaseID = "ID"
	// fieldVersionID specifies the name of the field with OS distribution version ID
	fieldVersionID = "VERSION_ID"
)

var (
	// The reason this is not a wildcard pattern as /etc/*release is that
	// there's a mixture of files with various degree of structure, so this
	// tries to follow a safe path of only looking at known structured files in
	// this group.
	releases = []string{
		"/etc/os-release",
		"/etc/debian_release",
	}
	versions = []string{
		"/etc/system-release",
		"/etc/debian_version",
	}
)

func (r releaseFiles) releaseInfo() (info *OSRelease, err error) {
	info, err = parseGenericRelease(r.release)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return info, nil
}

func (r releaseFiles) updateInfo(release *OSRelease) {
	if r.version == nil {
		return
	}
	version, err := parseVersionFile(r.version)
	if err != nil {
		log.Warnf("Failed to read version information: %v.", err)
		return
	}
	release.VersionID = version
}

// releaseFiles combines various ways to query Linux distribution
// release version information
type releaseFiles struct {
	// release is distribution-specific release file,
	// although /etc/os-release seems to be the standard location to relay
	// release information on modern Linux distributions.
	// See: http://0pointer.de/blog/projects/os-release
	release io.ReadCloser
	// version is an additional distribution-specific file with
	// details about a release (i.e. specific release version)
	version io.ReadCloser
	// lsbRelease returns the distribution release version information
	// using the lsb_release tool if available.
	// If the tool is not supported, this field is nil.
	lsbRelease func() (*OSRelease, error)
}

// lsbRelease determines release version information using "lsb_release" tool
func lsbRelease() (info *OSRelease, err error) {
	const tool = "lsb_release"
	if _, err = exec.LookPath(tool); err != nil {
		return nil, trace.NotFound("%v not found", tool)
	}

	toolCmd := func(args ...string) (out []byte, err error) {
		args = append(args, "--short")
		cmd := exec.Command(tool, args...)
		if out, err = cmd.CombinedOutput(); err != nil {
			return nil, trace.Wrap(err, "failed to obtain release: %s", out)
		}
		return bytes.TrimSpace(out), nil
	}

	// distributor
	id, err := toolCmd("--id")
	if err != nil {
		return nil, trace.Wrap(err, "failed to determine OS distributor: %s", id)
	}

	// release
	release, err := toolCmd("--release")
	if err != nil {
		return nil, trace.Wrap(err, "failed to determine OS release version: %s", release)
	}

	return &OSRelease{
		ID:        string(id),
		VersionID: string(release),
	}, nil
}

// parseGenericRelease parses a /etc/os-release file.
//
// The specified closer will be closed
func parseGenericRelease(rc io.ReadCloser) (info *OSRelease, err error) {
	defer rc.Close()
	info = &OSRelease{}
	s := bufio.NewScanner(rc)
	s.Split(bufio.ScanLines)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if len(line) == 0 {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		var value string
		if value, err = strconv.Unquote(parts[1]); err != nil {
			value = parts[1]
		}
		switch parts[0] {
		case fieldReleaseID:
			info.ID = value
		case fieldVersionID:
			info.VersionID = value
		}
	}

	if err := s.Err(); err != nil {
		return nil, trace.Wrap(err)
	}

	return info, nil
}

// parseVersionFile parses a distribution-specific version file with
// detailed information (i.e. /etc/system-release on RHEL-descendant
// distributions or /etc/debian_version on Debian-descendants).
//
// The specified closer will be closed
func parseVersionFile(rc io.ReadCloser) (version string, err error) {
	defer rc.Close()
	content, err := ioutil.ReadAll(rc)
	if err != nil {
		return "", trace.Wrap(err)
	}

	version = getReleaseVersion(string(content))
	if version == "" {
		log.Warnf("Unable to parse OS release version from %s.", content)
		return "", trace.BadParameter("unable to parse OS release version")
	}
	return version, nil
}

// getReleaseVersion extracts the version detail from the version file.
//
// For example, on CentOS, the following version information:
//   CentOS Linux release 7.3.1611 (Core)
//
// yields "7.3.1611" as the version
func getReleaseVersion(version string) string {
	result := versionRegexp.FindStringSubmatch(version)
	if len(result) == 0 {
		return ""
	}
	return result[1]
}

type osReleaseGetter func() (*OSRelease, error)

var versionRegexp = regexp.MustCompile(".*?([0-9\\.]+).*")

func file(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, trace.Wrap(trace.ConvertSystemError(err),
			"failed to open %q", path)
	}
	return f, nil
}

func openFirst(paths ...string) (io.ReadCloser, error) {
	for _, path := range paths {
		f, err := file(path)
		if err != nil {
			if !trace.IsNotFound(err) {
				log.Warnf("Failed to read %q: %v", path, err)
			}
			continue
		}
		return f, nil
	}
	return nil, trace.NotFound("no files found")
}
