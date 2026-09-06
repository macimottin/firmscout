package domain

// ReleaseType is the kind of thing a release actually is.
//
// This vocabulary exists because "everything is firmware" is the most common failure
// in catalogues of this sort, and it is a failure with practical consequences: a
// server has BIOS, BMC firmware, drivers and an appliance OS, and a user searching for
// one does not want the others. The type is required on every release and has no
// default. A collector that cannot determine the type emits ReleaseTypeUnknown, which
// routes to classification rather than guessing.
type ReleaseType string

const (
	ReleaseTypeFirmware            ReleaseType = "firmware"
	ReleaseTypeBIOS                ReleaseType = "bios"
	ReleaseTypeBMCFirmware         ReleaseType = "bmc_firmware"
	ReleaseTypeDriver              ReleaseType = "driver"
	ReleaseTypeOperatingSystem     ReleaseType = "operating_system"
	ReleaseTypeEmbeddedOS          ReleaseType = "embedded_os"
	ReleaseTypeApplicationSoftware ReleaseType = "application_software"
	ReleaseTypeSaaSRelease         ReleaseType = "saas_release"
	ReleaseTypeManagementPlatform  ReleaseType = "management_platform"
	ReleaseTypeSecurityUpdate      ReleaseType = "security_update"
	ReleaseTypeDocumentationOnly   ReleaseType = "documentation_only"
	ReleaseTypeUnknown             ReleaseType = "unknown"

	// ReleaseTypeAdvisory is a vendor security advisory document. It is not an installable
	// artifact, which is what separates it from ReleaseTypeSecurityUpdate: a security
	// update is a release that fixes a vulnerability, an advisory is the vendor's statement
	// that one exists. Collapsing the two would make the vocabulary unable to express the
	// difference on the first day a vendor publishes both.
	ReleaseTypeAdvisory ReleaseType = "advisory"
)

var releaseTypeNames = map[ReleaseType]string{
	ReleaseTypeFirmware:            "Firmware",
	ReleaseTypeBIOS:                "BIOS",
	ReleaseTypeBMCFirmware:         "BMC firmware",
	ReleaseTypeDriver:              "Device driver",
	ReleaseTypeOperatingSystem:     "Operating system",
	ReleaseTypeEmbeddedOS:          "Embedded operating system",
	ReleaseTypeApplicationSoftware: "Application software",
	ReleaseTypeSaaSRelease:         "SaaS release",
	ReleaseTypeManagementPlatform:  "Management platform",
	ReleaseTypeSecurityUpdate:      "Security update",
	ReleaseTypeDocumentationOnly:   "Documentation-only update",
	ReleaseTypeAdvisory:            "Security advisory",
	ReleaseTypeUnknown:             "Unknown",
}

// ValidReleaseType reports whether t is part of the declared vocabulary.
func ValidReleaseType(t ReleaseType) bool {
	_, ok := releaseTypeNames[t]
	return ok
}

// DisplayName returns the human-readable label for a release type.
func (t ReleaseType) DisplayName() string {
	if n, ok := releaseTypeNames[t]; ok {
		return n
	}
	return string(t)
}

// NeedsClassification reports whether a release of this type still requires a decision
// before it can be published with confidence.
func (t ReleaseType) NeedsClassification() bool { return t == ReleaseTypeUnknown }

// LifecycleStatus is a product's position in its commercial life.
type LifecycleStatus string

const (
	LifecycleActive       LifecycleStatus = "active"
	LifecycleMaintenance  LifecycleStatus = "maintenance"
	LifecycleEOLAnnounced LifecycleStatus = "eol_announced"
	LifecycleEOL          LifecycleStatus = "eol"
	LifecycleEOS          LifecycleStatus = "eos"
	LifecycleUnknown      LifecycleStatus = "unknown"
)

// ValidLifecycleStatus reports whether s is a declared lifecycle status.
func ValidLifecycleStatus(s LifecycleStatus) bool {
	switch s {
	case LifecycleActive, LifecycleMaintenance, LifecycleEOLAnnounced,
		LifecycleEOL, LifecycleEOS, LifecycleUnknown:
		return true
	}
	return false
}
