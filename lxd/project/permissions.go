package project

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
	deviceconfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/units"
)

// AllowInstanceCreation returns an error if any project-specific limit or
// restriction is violated when creating a new instance.
func AllowInstanceCreation(tx *db.ClusterTx, projectName string, req api.InstancesPost) error {
	project, profiles, instances, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}

	if project == nil {
		return nil
	}

	var instanceType instancetype.Type
	switch req.Type {
	case api.InstanceTypeContainer:
		instanceType = instancetype.Container
	case api.InstanceTypeVM:
		instanceType = instancetype.VM
	default:
		return fmt.Errorf("Unexpected instance type '%s'", instanceType)
	}

	err = checkInstanceCountLimit(project, len(instances), instanceType)
	if err != nil {
		return err
	}

	// Add the instance being created.
	instances = append(instances, db.Instance{
		Name:     req.Name,
		Profiles: req.Profiles,
		Config:   req.Config,
	})

	// Special case restriction checks on volatile.* keys.
	err = checkRestrictionsOnVolatileConfig(
		project, instanceType, req.Name, req.Config, map[string]string{})
	if err != nil {
		return err
	}

	err = checkRestrictionsAndAggregateLimits(tx, project, instances, profiles)
	if err != nil {
		return err
	}

	return nil
}

// Check that we have not reached the maximum number of instances for
// this type.
func checkInstanceCountLimit(project *api.Project, instanceCount int, instanceType instancetype.Type) error {
	var key string
	switch instanceType {
	case instancetype.Container:
		key = "limits.containers"
	case instancetype.VM:
		key = "limits.virtual-machines"
	default:
		return fmt.Errorf("Unexpected instance type '%s'", instanceType)
	}

	value, ok := project.Config[key]
	if ok {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 0 {
			return fmt.Errorf("Unexpected '%s' value: '%s'", key, value)
		}

		if instanceCount >= limit {
			return fmt.Errorf(
				"Reached maximum number of instances of type %s in project %s",
				instanceType, project.Name)
		}
	}

	return nil
}

// Check restrictions on setting volatile.* keys.
func checkRestrictionsOnVolatileConfig(project *api.Project, instanceType instancetype.Type, instanceName string, config, currentConfig map[string]string) error {
	if project.Config["restrict"] == "false" {
		return nil
	}

	var restrictedLowLevel string
	switch instanceType {
	case instancetype.Container:
		restrictedLowLevel = "restricted.containers.lowlevel"
	case instancetype.VM:
		restrictedLowLevel = "restricted.virtual-machines.lowlevel"
	}

	if project.Config[restrictedLowLevel] == "allow" {
		return nil
	}

	for key, value := range config {
		if !strings.HasPrefix(key, "volatile.") {
			continue
		}

		currentValue, ok := currentConfig[key]
		if !ok {
			return fmt.Errorf(
				"Setting %q on %s %q in project %q is forbidden",
				key, instanceType, instanceName, project.Name)
		}

		if currentValue != value {
			return fmt.Errorf(
				"Changing %q on %s %q in project %q is forbidden",
				key, instanceType, instanceName, project.Name)
		}
	}

	return nil
}

// Check that we would not violate the project limits or restrictions if we
// were to commit the given instances and profiles.
func checkRestrictionsAndAggregateLimits(tx *db.ClusterTx, project *api.Project, instances []db.Instance, profiles []db.Profile) error {
	// List of config keys for which we need to check aggregate values
	// across all project instances.
	aggregateKeys := []string{}
	isRestricted := false
	for key, value := range project.Config {
		if shared.StringInSlice(key, allAggregateLimits) {
			aggregateKeys = append(aggregateKeys, key)
			continue
		}

		if key == "restricted" && shared.IsTrue(value) {
			isRestricted = true
			continue
		}
	}

	if len(aggregateKeys) == 0 && !isRestricted {
		return nil
	}

	instances = expandInstancesConfigAndDevices(instances, profiles)

	err := checkAggregateLimits(project, instances, aggregateKeys)
	if err != nil {
		return err
	}

	if isRestricted {
		err = checkRestrictions(project, instances, profiles)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkAggregateLimits(project *api.Project, instances []db.Instance, aggregateKeys []string) error {
	if len(aggregateKeys) == 0 {
		return nil
	}

	totals, err := getTotalsAcrossInstances(instances, aggregateKeys)
	if err != nil {
		return err
	}

	for _, key := range aggregateKeys {
		parser := aggregateLimitConfigValueParsers[key]
		max, err := parser(project.Config[key])
		if err != nil {
			return err
		}

		if totals[key] > max {
			return fmt.Errorf(
				"Reached maximum aggregate value %s for %q in project %s",
				project.Config[key], key, project.Name)
		}
	}
	return nil
}

// Check that the project's restrictions are not violated across the given
// instances and profiles.
func checkRestrictions(project *api.Project, instances []db.Instance, profiles []db.Profile) error {
	containerConfigChecks := map[string]func(value string) error{}
	devicesChecks := map[string]func(value map[string]string) error{}

	allowContainerLowLevel := false
	allowVMLowLevel := false

	for _, key := range AllRestrictions {
		// Check if this particularl restriction is defined explicitly
		// in the project config. If not, use the default value.
		restrictionValue, ok := project.Config[key]
		if !ok {
			restrictionValue = defaultRestrictionsValues[key]
		}

		switch key {
		case "restricted.containers.nesting":
			containerConfigChecks["security.nesting"] = func(instanceValue string) error {
				if restrictionValue == "block" && shared.IsTrue(instanceValue) {
					return fmt.Errorf("Container nesting is forbidden")
				}

				return nil
			}
		case "restricted.containers.lowlevel":
			if restrictionValue == "allow" {
				allowContainerLowLevel = true
			}
		case "restricted.containers.privilege":
			containerConfigChecks["security.privileged"] = func(instanceValue string) error {
				if restrictionValue != "allow" && shared.IsTrue(instanceValue) {
					return fmt.Errorf("Privileged containers are forbidden")
				}

				return nil
			}
			containerConfigChecks["security.idmap.isolated"] = func(instanceValue string) error {
				if restrictionValue == "isolated" && !shared.IsTrue(instanceValue) {
					return fmt.Errorf("Non-isolated containers are forbidden")
				}

				return nil
			}
		case "restricted.virtual-machines.lowlevel":
			if restrictionValue == "allow" {
				allowVMLowLevel = true
			}
		case "restricted.devices.unix-char":
			devicesChecks["unix-char"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("Unix character devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.unix-block":
			devicesChecks["unix-block"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("Unix block devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.unix-hotplug":
			devicesChecks["unix-hotplug"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("Unix hotplug devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.infiniband":
			devicesChecks["infiniband"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("Infiniband devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.gpu":
			devicesChecks["gpu"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("GPU devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.usb":
			devicesChecks["usb"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("USB devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.nic":
			devicesChecks["nic"] = func(device map[string]string) error {
				switch restrictionValue {
				case "block":
					return fmt.Errorf("Network devices are forbidden")
				case "managed":
					if device["network"] == "" {
						return fmt.Errorf("Only managed network devices are allowed")
					}
				}
				return nil
			}
		case "restricted.devices.disk":
			devicesChecks["disk"] = func(device map[string]string) error {
				// The root device is always allowed.
				if device["path"] == "/" && device["pool"] != "" {
					return nil
				}

				switch restrictionValue {
				case "block":
					return fmt.Errorf("Disk devices are forbidden")
				case "managed":
					if device["pool"] == "" {
						return fmt.Errorf("Attaching disks not backed by a pool is forbidden")
					}
				}
				return nil
			}
		}
	}

	// Common config check logic between instances and profiles.
	entityConfigChecker := func(entityType, entityName string, config map[string]string) error {
		isContainerOrProfile := shared.StringInSlice(entityType, []string{"container", "profile"})
		isVMOrProfile := shared.StringInSlice(entityType, []string{"virtual machine", "profile"})
		for key, value := range config {
			// First check if the key is a forbidden low-level one.
			if isContainerOrProfile && !allowContainerLowLevel && isContainerLowLevelOptionForbidden(key) {
				return fmt.Errorf("Use of low-level config %q on %s %q of project %q is forbidden",
					key, entityType, entityName, project.Name)
			}

			if isVMOrProfile && !allowVMLowLevel && isVMLowLevelOptionForbidden(key) {
				return fmt.Errorf("Use of low-level config %q on %s %q of project %q is forbidden",
					key, entityType, entityName, project.Name)
			}

			var checker func(value string) error
			if isContainerOrProfile {
				checker = containerConfigChecks[key]
			}

			if checker == nil {
				continue
			}
			err := checker(value)
			if err != nil {
				return errors.Wrapf(
					err,
					"Invalid value %q for config %q on %s %q of project %q",
					value, key, entityType, entityName, project.Name)
			}
		}
		return nil
	}

	// Common devices check logic between instances and profiles.
	entityDevicesChecker := func(entityType, entityName string, devices map[string]map[string]string) error {
		for name, device := range devices {
			check, ok := devicesChecks[device["type"]]
			if !ok {
				continue
			}

			err := check(device)
			if err != nil {
				return errors.Wrapf(
					err,
					"Invalid device %q on %s %q of project %q",
					name, entityType, entityName, project.Name)
			}
		}
		return nil
	}

	for _, instance := range instances {
		var err error
		switch instance.Type {
		case instancetype.Container:
			err = entityConfigChecker("container", instance.Name, instance.Config)
		case instancetype.VM:
			err = entityConfigChecker("virtual machine", instance.Name, instance.Config)
		}
		if err != nil {
			return err
		}

		err = entityDevicesChecker("instance", instance.Name, instance.Devices)
		if err != nil {
			return err
		}
	}

	for _, profile := range profiles {
		err := entityConfigChecker("profile", profile.Name, profile.Config)
		if err != nil {
			return err
		}

		err = entityDevicesChecker("profile", profile.Name, profile.Devices)
		if err != nil {
			return err
		}
	}

	return nil
}

var allAggregateLimits = []string{
	"limits.cpu",
	"limits.memory",
	"limits.processes",
}

// AllRestrictions lists all available 'restrict.*' config keys.
var AllRestrictions = []string{
	"restricted.containers.nesting",
	"restricted.containers.lowlevel",
	"restricted.containers.privilege",
	"restricted.virtual-machines.lowlevel",
	"restricted.devices.unix-char",
	"restricted.devices.unix-block",
	"restricted.devices.unix-hotplug",
	"restricted.devices.infiniband",
	"restricted.devices.gpu",
	"restricted.devices.usb",
	"restricted.devices.nic",
	"restricted.devices.disk",
}

var defaultRestrictionsValues = map[string]string{
	"restricted.containers.nesting":        "block",
	"restricted.containers.lowlevel":       "block",
	"restricted.containers.privilege":      "unprivileged",
	"restricted.virtual-machines.lowlevel": "block",
	"restricted.devices.unix-char":         "block",
	"restricted.devices.unix-block":        "block",
	"restricted.devices.unix-hotplug":      "block",
	"restricted.devices.infiniband":        "block",
	"restricted.devices.gpu":               "block",
	"restricted.devices.usb":               "block",
	"restricted.devices.nic":               "managed",
	"restricted.devices.disk":              "managed",
}

// Return true if a low-level container option is forbidden.
func isContainerLowLevelOptionForbidden(key string) bool {
	if strings.HasPrefix(key, "security.syscalls") {
		return true
	}

	if shared.StringInSlice(key, []string{
		"boot.host_shutdown_timeout",
		"linux.kernel_modules",
		"raw.apparmor",
		"raw.idmap",
		"raw.lxc",
		"raw.seccomp",
		"security.devlxd.images",
		"security.idmap.base",
		"security.idmap.size",
	}) {
		return true
	}

	return false
}

// Return true if a low-level VM option is forbidden.
func isVMLowLevelOptionForbidden(key string) bool {
	if shared.StringInSlice(key, []string{
		"boot.host_shutdown_timeout",
		"limits.memory.hugepages",
		"raw.qemu",
	}) {
		return true
	}
	return false
}

// AllowInstanceUpdate returns an error if any project-specific limit or
// restriction is violated when updating an existing instance.
func AllowInstanceUpdate(tx *db.ClusterTx, projectName, instanceName string, req api.InstancePut, currentConfig map[string]string) error {
	var updatedInstance *db.Instance
	project, profiles, instances, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}

	if project == nil {
		return nil
	}

	// Change the instance being updated.
	for i, instance := range instances {
		if instance.Name != instanceName {
			continue
		}
		instances[i].Profiles = req.Profiles
		instances[i].Config = req.Config
		instances[i].Devices = req.Devices
		updatedInstance = &instances[i]
	}

	// Special case restriction checks on volatile.* keys, since we want to
	// detect if they were changed or added.
	err = checkRestrictionsOnVolatileConfig(
		project, updatedInstance.Type, updatedInstance.Name, req.Config, currentConfig)
	if err != nil {
		return err
	}

	err = checkRestrictionsAndAggregateLimits(tx, project, instances, profiles)
	if err != nil {
		return err
	}

	return nil
}

// AllowProfileUpdate checks that project limits and restrictions are not
// violated when changing a profile.
func AllowProfileUpdate(tx *db.ClusterTx, projectName, profileName string, req api.ProfilePut) error {
	project, profiles, instances, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}
	if project == nil {
		return nil
	}

	// Change the profile being updated.
	for i, profile := range profiles {
		if profile.Name != profileName {
			continue
		}
		profiles[i].Config = req.Config
		profiles[i].Devices = req.Devices
	}

	err = checkRestrictionsAndAggregateLimits(tx, project, instances, profiles)
	if err != nil {
		return err
	}

	return nil
}

// AllowProjectUpdate checks the new config to be set on a project is valid.
func AllowProjectUpdate(tx *db.ClusterTx, projectName string, config map[string]string, changed []string) error {
	_, profiles, instances, err := fetchProject(tx, projectName, false)
	if err != nil {
		return err
	}

	instances = expandInstancesConfigAndDevices(instances, profiles)

	// List of keys that need to check aggregate values across all project
	// instances.
	aggregateKeys := []string{}

	for _, key := range changed {
		if strings.HasPrefix(key, "restricted.") {
			project := &api.Project{
				Name: projectName,
				ProjectPut: api.ProjectPut{
					Config: config,
				},
			}

			err := checkRestrictions(project, instances, profiles)
			if err != nil {
				return errors.Wrapf(err, "Conflict detected when changing %q in project %q", key, projectName)
			}

			continue
		}

		switch key {
		case "limits.containers":
			fallthrough
		case "limits.virtual-machines":
			err := validateInstanceCountLimit(instances, key, config[key], projectName)
			if err != nil {
				return errors.Wrapf(err, "Can't change %q in project %q", key, projectName)
			}
		case "limits.processes":
			fallthrough
		case "limits.cpu":
			fallthrough
		case "limits.memory":
			aggregateKeys = append(aggregateKeys, key)

		}
	}

	if len(aggregateKeys) > 0 {
		totals, err := getTotalsAcrossInstances(instances, aggregateKeys)
		if err != nil {
			return err
		}
		for _, key := range aggregateKeys {
			err := validateAggregateLimit(totals, key, config[key])
			if err != nil {
				return err
			}
		}

	}

	return nil
}

// Check that limits.containers or limits.virtual-machines is equal or above
// the current count.
func validateInstanceCountLimit(instances []db.Instance, key, value, project string) error {
	instanceType := countConfigInstanceType[key]
	limit, err := strconv.Atoi(value)
	if err != nil {
		return err
	}

	dbType, err := instancetype.New(string(instanceType))
	if err != nil {
		return err
	}

	count := 0
	for _, instance := range instances {
		if instance.Type == dbType {
			count++
		}
	}

	if limit < count {
		return fmt.Errorf(
			"'%s' is too low: there currently are %d instances of type %s in project %s",
			key, count, instanceType, project)
	}

	return nil
}

var countConfigInstanceType = map[string]api.InstanceType{
	"limits.containers":       api.InstanceTypeContainer,
	"limits.virtual-machines": api.InstanceTypeVM,
}

// Validates an aggregate limit, checking that the new value is not below the
// current total amount.
func validateAggregateLimit(totals map[string]int64, key, value string) error {
	parser := aggregateLimitConfigValueParsers[key]
	limit, err := parser(value)
	if err != nil {
		errors.Wrapf(err, "Invalid value '%s' for limit %s", value, key)
	}

	total := totals[key]
	if limit < total {
		printer := aggregateLimitConfigValuePrinters[key]
		return fmt.Errorf("'%s' is too low: current total is %s", key, printer(total))
	}

	return nil
}

// Return true if the project has some limits or restrictions set.
func projectHasLimitsOrRestrictions(project *api.Project) bool {
	for k, v := range project.Config {
		if strings.HasPrefix(k, "limits.") {
			return true
		}

		if k == "restricted" && shared.IsTrue(v) {
			return true
		}

	}
	return false
}

// Fetch the given project from the database along with its profiles and instances.
//
// If the skipIfNoLimits flag is true, profiles and instances won't be loaded
// if the profile has no limits set on it, and nil will be returned.
func fetchProject(tx *db.ClusterTx, projectName string, skipIfNoLimits bool) (*api.Project, []db.Profile, []db.Instance, error) {
	project, err := tx.ProjectGet(projectName)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "Fetch project database object")
	}

	if skipIfNoLimits && !projectHasLimitsOrRestrictions(project) {
		return nil, nil, nil, nil
	}

	profilesFilter := db.ProfileFilter{}

	// If the project has the profiles feature enabled, we use its own
	// profiles to expand the instances configs, otherwise we use the
	// profiles from the default project.
	if projectName == Default || shared.IsTrue(project.Config["features.profiles"]) {
		profilesFilter.Project = projectName
	} else {
		profilesFilter.Project = Default
	}

	profiles, err := tx.ProfileList(profilesFilter)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "Fetch profiles from database")
	}

	instances, err := tx.InstanceList(db.InstanceFilter{Project: projectName})
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "Fetch project instances from database")
	}

	return project, profiles, instances, nil
}

// Expand the configuration and devices of the given instances, taking the give
// project profiles into account.
func expandInstancesConfigAndDevices(instances []db.Instance, profiles []db.Profile) []db.Instance {
	expandedInstances := make([]db.Instance, len(instances))

	// Index of all profiles by name.
	profilesByName := map[string]db.Profile{}
	for _, profile := range profiles {
		profilesByName[profile.Name] = profile
	}

	for i, instance := range instances {
		profiles := make([]api.Profile, len(instance.Profiles))

		for j, name := range instance.Profiles {
			profile := profilesByName[name]
			profiles[j] = *db.ProfileToAPI(&profile)
		}

		expandedInstances[i] = instance
		expandedInstances[i].Config = db.ProfilesExpandConfig(instance.Config, profiles)
		expandedInstances[i].Devices = db.ProfilesExpandDevices(
			deviceconfig.NewDevices(instance.Devices), profiles).CloneNative()
	}

	return expandedInstances
}

// Sum of the effective instance-level value for the given limits across all
// project instances. If excludeInstance is not the empty string, exclude the
// instance with that name.
func getTotalsAcrossInstances(instances []db.Instance, keys []string) (map[string]int64, error) {
	totals := map[string]int64{}

	for _, key := range keys {
		totals[key] = 0
	}

	for _, instance := range instances {
		limits, err := getInstanceLimits(instance, keys)
		if err != nil {
			return nil, err
		}

		for _, key := range keys {
			totals[key] += limits[key]
		}
	}

	return totals, nil
}

// Return the effective instance-level values for the limits with the given
// keys.
func getInstanceLimits(instance db.Instance, keys []string) (map[string]int64, error) {
	limits := map[string]int64{}

	for _, key := range keys {
		value, ok := instance.Config[key]
		if !ok || value == "" {
			return nil, fmt.Errorf(
				"Instance %s in project %s has no '%s' config, either directly or via a profile",
				instance.Name, instance.Project, key)
		}

		parser := aggregateLimitConfigValueParsers[key]
		limit, err := parser(value)
		if err != nil {
			return nil, errors.Wrapf(
				err, "Parse '%s' for instance %s in project %s",
				key, instance.Name, instance.Project)
		}

		limits[key] = limit
	}

	return limits, nil
}

var aggregateLimitConfigValueParsers = map[string]func(string) (int64, error){
	"limits.memory": func(value string) (int64, error) {
		if strings.HasSuffix(value, "%") {
			return -1, fmt.Errorf("Value can't be a percentage")
		}
		return units.ParseByteSizeString(value)
	},
	"limits.processes": func(value string) (int64, error) {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return -1, err
		}
		return int64(limit), nil
	},
	"limits.cpu": func(value string) (int64, error) {
		if strings.Contains(value, ",") || strings.Contains(value, "-") {
			return -1, fmt.Errorf("CPUs can't be pinned if project limits are used")
		}

		limit, err := strconv.Atoi(value)
		if err != nil {
			return -1, err
		}

		return int64(limit), nil
	},
}

var aggregateLimitConfigValuePrinters = map[string]func(int64) string{
	"limits.memory": func(limit int64) string {
		return units.GetByteSizeString(limit, 1)
	},
	"limits.processes": func(limit int64) string {
		return fmt.Sprintf("%d", limit)
	},
	"limits.cpu": func(limit int64) string {
		return fmt.Sprintf("%d", limit)
	},
}
