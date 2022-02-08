package networking

import (
	"context"
	"fmt"
	awssdk "github.com/aws/aws-sdk-go/aws"
	ec2sdk "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/aws/services"
	elbv2model "sigs.k8s.io/aws-load-balancer-controller/pkg/model/elbv2"
	"sort"
	"strings"
)

const (
	TagKeySubnetInternalELB = "kubernetes.io/role/internal-elb"
	TagKeySubnetPublicELB   = "kubernetes.io/role/elb"
)

type subnetLocaleType string

const (
	subnetLocaleTypeAvailabilityZone subnetLocaleType = "availability-zone"
	subnetLocaleTypeLocalZone        subnetLocaleType = "local-zone"
	subnetLocaleTypeWavelengthZone   subnetLocaleType = "wavelength-zone"
	subnetLocaleTypeOutpost          subnetLocaleType = "outpost"
)

const (
	zoneTypeAvailabilityZone string = "availability-zone"
	zoneTypeLocalZone        string = "local-zone"
	zoneTypeWavelengthZone   string = "wavelength-zone"
)

// options for resolve subnets.
type SubnetsResolveOptions struct {
	// The Load Balancer Type.
	// By default, it's ALB.
	LBType elbv2model.LoadBalancerType
	// The Load Balancer Scheme.
	// By default, it's internet-facing.
	LBScheme elbv2model.LoadBalancerScheme
	// count of available ip addresses
	AvailableIPAddressCount int64
}

// ApplyOptions applies slice of SubnetsResolveOption.
func (opts *SubnetsResolveOptions) ApplyOptions(options []SubnetsResolveOption) {
	for _, option := range options {
		option(opts)
	}
}

// defaultSubnetsResolveOptions generates the default SubnetsResolveOptions
func defaultSubnetsResolveOptions() SubnetsResolveOptions {
	return SubnetsResolveOptions{
		LBType:   elbv2model.LoadBalancerTypeApplication,
		LBScheme: elbv2model.LoadBalancerSchemeInternetFacing,
	}
}

type SubnetsResolveOption func(opts *SubnetsResolveOptions)

// WithSubnetsResolveLBType generates a option that configures LBType.
func WithSubnetsResolveLBType(lbType elbv2model.LoadBalancerType) SubnetsResolveOption {
	return func(opts *SubnetsResolveOptions) {
		opts.LBType = lbType
	}
}

// WithSubnetsResolveLBScheme generates a option that configures LBScheme.
func WithSubnetsResolveLBScheme(lbScheme elbv2model.LoadBalancerScheme) SubnetsResolveOption {
	return func(opts *SubnetsResolveOptions) {
		opts.LBScheme = lbScheme
	}
}

// WithSubnetsResolveAvailableIPAddressCount generates a option that configures AvailableIPAddressCount.
func WithSubnetsResolveAvailableIPAddressCount(AvailableIPAddressCount int64) SubnetsResolveOption {
	return func(opts *SubnetsResolveOptions) {
		opts.AvailableIPAddressCount = AvailableIPAddressCount
	}
}

// SubnetsResolver is responsible for resolve EC2 Subnets for Load Balancers.
type SubnetsResolver interface {
	// ResolveViaDiscovery resolve subnets by auto discover matching subnets.
	// Discovery candidate includes all subnets within clusterVPC that contains the "kubernetes.io/cluster/<cluster-name>" tag.
	// Additionally,
	//   * for internet-facing Load Balancer, "kubernetes.io/role/elb" tag must presents.
	//   * for internal Load Balancer, "kubernetes.io/role/internal-elb" tag must presents.
	// If multiple subnets are found for specific AZ, one subnet is chosen based on the lexical order of subnetID.
	ResolveViaDiscovery(ctx context.Context, opts ...SubnetsResolveOption) ([]*ec2sdk.Subnet, error)

	// ResolveViaNameOrIDSlice resolve subnets using subnet name or ID.
	ResolveViaNameOrIDSlice(ctx context.Context, subnetNameOrIDs []string, opts ...SubnetsResolveOption) ([]*ec2sdk.Subnet, error)
}

// NewDefaultSubnetsResolver constructs new defaultSubnetsResolver.
func NewDefaultSubnetsResolver(azInfoProvider AZInfoProvider, ec2Client services.EC2, vpcID string, clusterName string, logger logr.Logger, desiredAvailabilityZoneIDs string) *defaultSubnetsResolver {
	var azIds []string
	if len(desiredAvailabilityZoneIDs) > 0 {
		azIds = strings.Split(desiredAvailabilityZoneIDs, ",")
	}
	return &defaultSubnetsResolver{
		azInfoProvider:             azInfoProvider,
		ec2Client:                  ec2Client,
		vpcID:                      vpcID,
		clusterName:                clusterName,
		logger:                     logger,
		desiredAvailabilityZoneIDs: azIds,
	}
}

var _ SubnetsResolver = &defaultSubnetsResolver{}

// default implementation for SubnetsResolver.
type defaultSubnetsResolver struct {
	azInfoProvider             AZInfoProvider
	ec2Client                  services.EC2
	vpcID                      string
	clusterName                string
	logger                     logr.Logger
	desiredAvailabilityZoneIDs []string
}

func (r *defaultSubnetsResolver) ResolveViaDiscovery(ctx context.Context, opts ...SubnetsResolveOption) ([]*ec2sdk.Subnet, error) {
	resolveOpts := defaultSubnetsResolveOptions()
	resolveOpts.ApplyOptions(opts)

	subnetRoleTagKey := ""
	switch resolveOpts.LBScheme {
	case elbv2model.LoadBalancerSchemeInternal:
		subnetRoleTagKey = TagKeySubnetInternalELB
	case elbv2model.LoadBalancerSchemeInternetFacing:
		subnetRoleTagKey = TagKeySubnetPublicELB
	}
	req := &ec2sdk.DescribeSubnetsInput{Filters: []*ec2sdk.Filter{
		{
			Name:   awssdk.String("tag:" + subnetRoleTagKey),
			Values: awssdk.StringSlice([]string{"", "1"}),
		},
		{
			Name:   awssdk.String("vpc-id"),
			Values: awssdk.StringSlice([]string{r.vpcID}),
		},
	}}

	allSubnets, err := r.ec2Client.DescribeSubnetsAsList(ctx, req)
	if err != nil {
		return nil, err
	}
	var subnets []*ec2sdk.Subnet
	for _, subnet := range allSubnets {
		if r.checkSubnetIsNotTaggedForOtherClusters(subnet) {
			subnets = append(subnets, subnet)
		}
	}
	filteredSubnets := r.filterSubnetsByAvailableZoneIDs(subnets, r.desiredAvailabilityZoneIDs)
	filteredSubnets = r.filterSubnetsByAvailableIPAddress(filteredSubnets, resolveOpts.AvailableIPAddressCount)

	subnetsByAZ := mapSDKSubnetsByAZ(filteredSubnets)
	chosenSubnets := make([]*ec2sdk.Subnet, 0, len(subnetsByAZ))
	for az, subnets := range subnetsByAZ {
		if len(subnets) == 1 {
			chosenSubnets = append(chosenSubnets, subnets[0])
		} else if len(subnets) > 1 {
			sort.Slice(subnets, func(i, j int) bool {
				clusterTagI := r.checkSubnetHasClusterTag(subnets[i])
				clusterTagJ := r.checkSubnetHasClusterTag(subnets[j])
				if clusterTagI != clusterTagJ {
					if clusterTagI {
						return true
					}
					return false
				}
				return awssdk.StringValue(subnets[i].SubnetId) < awssdk.StringValue(subnets[j].SubnetId)
			})
			r.logger.Info("multiple subnet in the same AvailabilityZone", "AvailabilityZone", az,
				"chosen", subnets[0].SubnetId, "ignored", subnets[1:])
			chosenSubnets = append(chosenSubnets, subnets[0])
		}
	}
	if len(chosenSubnets) == 0 {
		return nil, errors.New("unable to discover at least one subnet")
	}
	subnetLocale, err := r.validateSubnetsLocaleUniformity(ctx, chosenSubnets)
	if err != nil {
		return nil, err
	}
	if err := r.validateSubnetsMinimalCount(chosenSubnets, subnetLocale, resolveOpts); err != nil {
		return nil, err
	}
	sortSubnetsByID(chosenSubnets)
	return chosenSubnets, nil
}

func (r *defaultSubnetsResolver) ResolveViaNameOrIDSlice(ctx context.Context, subnetNameOrIDs []string, opts ...SubnetsResolveOption) ([]*ec2sdk.Subnet, error) {
	resolveOpts := defaultSubnetsResolveOptions()
	resolveOpts.ApplyOptions(opts)

	var subnetIDs []string
	var subnetNames []string
	for _, nameOrID := range subnetNameOrIDs {
		if strings.HasPrefix(nameOrID, "subnet-") {
			subnetIDs = append(subnetIDs, nameOrID)
		} else {
			subnetNames = append(subnetNames, nameOrID)
		}
	}
	var resolvedSubnets []*ec2sdk.Subnet
	if len(subnetIDs) > 0 {
		req := &ec2sdk.DescribeSubnetsInput{
			SubnetIds: awssdk.StringSlice(subnetIDs),
		}
		subnets, err := r.ec2Client.DescribeSubnetsAsList(ctx, req)
		if err != nil {
			return nil, err
		}
		resolvedSubnets = append(resolvedSubnets, subnets...)
	}

	if len(subnetNames) > 0 {
		req := &ec2sdk.DescribeSubnetsInput{
			Filters: []*ec2sdk.Filter{
				{
					Name:   awssdk.String("tag:Name"),
					Values: awssdk.StringSlice(subnetNames),
				},
				{
					Name:   awssdk.String("vpc-id"),
					Values: awssdk.StringSlice([]string{r.vpcID}),
				},
			},
		}
		subnets, err := r.ec2Client.DescribeSubnetsAsList(ctx, req)
		if err != nil {
			return nil, err
		}
		resolvedSubnets = append(resolvedSubnets, subnets...)
	}
	if len(resolvedSubnets) != len(subnetNameOrIDs) {
		return nil, errors.Errorf("couldn't find all subnets, nameOrIDs: %v, found: %v", subnetNameOrIDs, len(resolvedSubnets))
	}

	resolvedSubnets = r.filterSubnetsByAvailableZoneIDs(resolvedSubnets, r.desiredAvailabilityZoneIDs)
	if len(resolvedSubnets) == 0 {
		return nil, errors.New("unable to resolve at least one subnet")
	}

	if err := r.validateSubnetsAZExclusivity(resolvedSubnets); err != nil {
		return nil, err
	}
	subnetLocale, err := r.validateSubnetsLocaleUniformity(ctx, resolvedSubnets)
	if err != nil {
		return nil, err
	}
	if err := r.validateSubnetsMinimalCount(resolvedSubnets, subnetLocale, resolveOpts); err != nil {
		return nil, err
	}
	sortSubnetsByID(resolvedSubnets)
	return resolvedSubnets, nil
}

// validateSDKSubnetsAZExclusivity validates subnets belong to different AZs.
// subnets passed-in must be non-empty
func (r *defaultSubnetsResolver) validateSubnetsAZExclusivity(subnets []*ec2sdk.Subnet) error {
	subnetsByAZ := mapSDKSubnetsByAZ(subnets)
	for az, subnets := range subnetsByAZ {
		if len(subnets) > 1 {
			subnetIDs := make([]string, 0, len(subnets))
			for _, subnet := range subnets {
				subnetIDs = append(subnetIDs, awssdk.StringValue(subnet.SubnetId))
			}
			return errors.Errorf("multiple subnets in same Availability Zone %v: %v", az, subnetIDs)
		}
	}
	return nil
}

// validateSDKSubnetsLocaleExclusivity validates all subnets belong to same locale, and returns the same locale.
// subnets passed-in must be non-empty
func (r *defaultSubnetsResolver) validateSubnetsLocaleUniformity(ctx context.Context, subnets []*ec2sdk.Subnet) (subnetLocaleType, error) {
	subnetLocales := sets.NewString()
	for _, subnet := range subnets {
		subnetLocale, err := r.buildSDKSubnetLocaleType(ctx, subnet)
		if err != nil {
			return "", err
		}
		subnetLocales.Insert(string(subnetLocale))
	}
	if len(subnetLocales) > 1 {
		return "", errors.Errorf("subnets in multiple locales: %v", subnetLocales.List())
	}
	subnetLocale, _ := subnetLocales.PopAny()
	return subnetLocaleType(subnetLocale), nil
}

// validateSubnetsMinimalCount validates subnets meets minimal count requirement.
func (r *defaultSubnetsResolver) validateSubnetsMinimalCount(subnets []*ec2sdk.Subnet, subnetLocale subnetLocaleType, resolveOpts SubnetsResolveOptions) error {
	minimalCount := r.computeSubnetsMinimalCount(subnetLocale, resolveOpts)
	if len(subnets) < minimalCount {
		return errors.Errorf("subnets count less than minimal required count: %v < %v", len(subnets), minimalCount)
	}
	return nil
}

// computeSubnetsMinimalCount returns the minimal count requirement for subnets.
func (r *defaultSubnetsResolver) computeSubnetsMinimalCount(subnetLocale subnetLocaleType, resolveOpts SubnetsResolveOptions) int {
	minimalCount := 1
	if resolveOpts.LBType == elbv2model.LoadBalancerTypeApplication && subnetLocale == subnetLocaleTypeAvailabilityZone {
		minimalCount = 2
	}
	return minimalCount
}

// buildSDKSubnetLocaleType builds the locale type for subnet.
func (r *defaultSubnetsResolver) buildSDKSubnetLocaleType(ctx context.Context, subnet *ec2sdk.Subnet) (subnetLocaleType, error) {
	if subnet.OutpostArn != nil && len(*subnet.OutpostArn) != 0 {
		return subnetLocaleTypeOutpost, nil
	}
	subnetAZID := awssdk.StringValue(subnet.AvailabilityZoneId)
	azInfoByAZID, err := r.azInfoProvider.FetchAZInfos(ctx, []string{subnetAZID})
	if err != nil {
		return "", err
	}
	subnetAZInfo := azInfoByAZID[subnetAZID]
	subnetZoneType := awssdk.StringValue(subnetAZInfo.ZoneType)
	switch subnetZoneType {
	case zoneTypeAvailabilityZone:
		return subnetLocaleTypeAvailabilityZone, nil
	case zoneTypeLocalZone:
		return subnetLocaleTypeLocalZone, nil
	case zoneTypeWavelengthZone:
		return subnetLocaleTypeWavelengthZone, nil
	default:
		return "", errors.Errorf("unknown zone type for subnet %v: %v", awssdk.StringValue(subnet.SubnetId), subnetZoneType)
	}
}

// checkSubnetHasClusterTag checks if the subnet is tagged for the current cluster
func (r *defaultSubnetsResolver) checkSubnetHasClusterTag(subnet *ec2sdk.Subnet) bool {
	clusterResourceTagKey := fmt.Sprintf("kubernetes.io/cluster/%s", r.clusterName)
	for _, tag := range subnet.Tags {
		if clusterResourceTagKey == awssdk.StringValue(tag.Key) {
			return true
		}
	}
	return false
}

// checkSubnetIsNotTaggedForOtherClusters checks whether the subnet is tagged for the current cluster
// or it doesn't contain the cluster tag at all. If the subnet contains a tag for other clusters, then
// this check returns false so that the subnet does not used for the load balancer.
func (r *defaultSubnetsResolver) checkSubnetIsNotTaggedForOtherClusters(subnet *ec2sdk.Subnet) bool {
	clusterResourceTagPrefix := "kubernetes.io/cluster"
	clusterResourceTagKey := fmt.Sprintf("kubernetes.io/cluster/%s", r.clusterName)
	hasClusterResourceTagPrefix := false
	for _, tag := range subnet.Tags {
		tagKey := awssdk.StringValue(tag.Key)
		if tagKey == clusterResourceTagKey {
			return true
		}
		if strings.HasPrefix(tagKey, clusterResourceTagPrefix) {
			// If the cluster tag is for a different cluster, keep track of it and exclude
			// the subnet if no matching tag found for the current cluster.
			hasClusterResourceTagPrefix = true
		}
	}
	if hasClusterResourceTagPrefix {
		return false
	}
	return true
}

// mapSDKSubnetsByAZ builds the subnets slice by AZ mapping.
func mapSDKSubnetsByAZ(subnets []*ec2sdk.Subnet) map[string][]*ec2sdk.Subnet {
	subnetsByAZ := make(map[string][]*ec2sdk.Subnet)
	for _, subnet := range subnets {
		subnetAZ := awssdk.StringValue(subnet.AvailabilityZone)
		subnetsByAZ[subnetAZ] = append(subnetsByAZ[subnetAZ], subnet)
	}
	return subnetsByAZ
}

// sortSubnetsByID sorts given subnets slice by subnetID.
func sortSubnetsByID(subnets []*ec2sdk.Subnet) {
	sort.Slice(subnets, func(i, j int) bool {
		return awssdk.StringValue(subnets[i].SubnetId) < awssdk.StringValue(subnets[j].SubnetId)
	})
}

func (r *defaultSubnetsResolver) filterSubnetsByAvailableIPAddress(subnets []*ec2sdk.Subnet, availableIPAddressCount int64) []*ec2sdk.Subnet {
	filteredSubnets := make([]*ec2sdk.Subnet, 0, len(subnets))
	for _, subnet := range subnets {
		if awssdk.Int64Value(subnet.AvailableIpAddressCount) >= availableIPAddressCount {
			filteredSubnets = append(filteredSubnets, subnet)
		} else {
			r.logger.Info("ELB requires at least 8 free IP addresses in each subnet",
				"not enough IP addresses found in ", awssdk.StringValue(subnet.SubnetId))
		}
	}
	return filteredSubnets
}

func (r *defaultSubnetsResolver) filterSubnetsByAvailableZoneIDs(subnets []*ec2sdk.Subnet, DesiredAvailabilityZoneIDs []string) []*ec2sdk.Subnet {
	if DesiredAvailabilityZoneIDs != nil && len(DesiredAvailabilityZoneIDs) > 0 {
		filteredSubnets := make([]*ec2sdk.Subnet, 0, len(subnets))
		for _, subnet := range subnets {
			matched := false
			zoneId := awssdk.StringValue(subnet.AvailabilityZoneId)
			for _, azId := range DesiredAvailabilityZoneIDs {
				if strings.EqualFold(zoneId, azId) {
					filteredSubnets = append(filteredSubnets, subnet)
					matched = true
					break
				}
			}
			if !matched {
				r.logger.Info("ELB requires the subnet %s @ %s in desired availability zone IDs %v",
					subnet.SubnetId, zoneId, DesiredAvailabilityZoneIDs)
			}
		}
		return filteredSubnets
	} else {
		return subnets
	}
}
