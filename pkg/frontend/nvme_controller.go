// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2022-2023 Dell Inc, or its subsidiaries.
// Copyright (C) 2022 Marvell International Ltd.
// Copyright (C) 2023 Intel Corporation

// Package frontend implememnts the FrontEnd APIs (host facing) of the storage Server
package frontend

import (
	"context"
	"fmt"
	"log"
	"path"
	"sort"

	pb "github.com/opiproject/opi-api/storage/v1alpha1/gen/go"
	"github.com/opiproject/opi-marvell-bridge/pkg/models"
	"github.com/opiproject/opi-spdk-bridge/pkg/server"

	"github.com/google/uuid"
	"go.einride.tech/aip/fieldbehavior"
	"go.einride.tech/aip/fieldmask"
	"go.einride.tech/aip/resourceid"
	"go.einride.tech/aip/resourcename"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func sortNvmeControllers(controllers []*pb.NvmeController) {
	sort.Slice(controllers, func(i int, j int) bool {
		return controllers[i].Spec.NvmeControllerId < controllers[j].Spec.NvmeControllerId
	})
}

// CreateNvmeController creates an Nvme controller
func (s *Server) CreateNvmeController(_ context.Context, in *pb.CreateNvmeControllerRequest) (*pb.NvmeController, error) {
	log.Printf("CreateNvmeController: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// check input parameters validity
	if in.NvmeController.Spec == nil || in.NvmeController.Spec.SubsystemId == nil || in.NvmeController.Spec.SubsystemId.Value == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid input subsystem parameters")
	}
	// see https://google.aip.dev/133#user-specified-ids
	resourceID := resourceid.NewSystemGenerated()
	if in.NvmeControllerId != "" {
		err := resourceid.ValidateUserSettable(in.NvmeControllerId)
		if err != nil {
			log.Printf("error: %v", err)
			return nil, err
		}
		log.Printf("client provided the ID of a resource %v, ignoring the name field %v", in.NvmeControllerId, in.NvmeController.Name)
		resourceID = in.NvmeControllerId
	}
	in.NvmeController.Name = server.ResourceIDToVolumeName(resourceID)
	// idempotent API when called with same key, should return same object
	controller, ok := s.Controllers[in.NvmeController.Name]
	if ok {
		log.Printf("Already existing NvmeController with id %v", in.NvmeController.Name)
		return controller, nil
	}
	// not found, so create a new one
	subsys, ok := s.Subsystems[in.NvmeController.Spec.SubsystemId.Value]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.NvmeController.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	params := models.MrvlNvmSubsysCreateCtrlrParams{
		Subnqn:       subsys.Spec.Nqn,
		PcieDomainID: int(in.NvmeController.Spec.PcieId.PortId),
		PfID:         int(in.NvmeController.Spec.PcieId.PhysicalFunction),
		VfID:         int(in.NvmeController.Spec.PcieId.VirtualFunction),
		CtrlrID:      int(in.NvmeController.Spec.NvmeControllerId),
		MaxNsq:       int(in.NvmeController.Spec.MaxNsq),
		MaxNcq:       int(in.NvmeController.Spec.MaxNcq),
		Mqes:         int(in.NvmeController.Spec.Sqes),
	}
	var result models.MrvlNvmSubsysCreateCtrlrResult
	err := s.rpc.Call("mrvl_nvm_subsys_create_ctrlr", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if result.Status != 0 {
		msg := fmt.Sprintf("Could not create CTRL: %s", in.NvmeController.Name)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	response := server.ProtoClone(in.NvmeController)
	response.Spec.NvmeControllerId = int32(result.CtrlrID)
	response.Status = &pb.NvmeControllerStatus{Active: true}
	s.Controllers[in.NvmeController.Name] = response
	return response, nil
}

// DeleteNvmeController deletes an Nvme controller
func (s *Server) DeleteNvmeController(_ context.Context, in *pb.DeleteNvmeControllerRequest) (*emptypb.Empty, error) {
	log.Printf("DeleteNvmeController: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	controller, ok := s.Controllers[in.Name]
	if !ok {
		if in.AllowMissing {
			return &emptypb.Empty{}, nil
		}
		return nil, fmt.Errorf("error finding controller %s", in.Name)
	}
	subsys, ok := s.Subsystems[controller.Spec.SubsystemId.Value]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", controller.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	params := models.MrvlNvmSubsysRemoveCtrlrParams{
		Subnqn:  subsys.Spec.Nqn,
		CtrlrID: int(controller.Spec.NvmeControllerId),
		Force:   1,
	}
	var result models.MrvlNvmSubsysRemoveCtrlrResult
	err := s.rpc.Call("mrvl_nvm_subsys_remove_ctrlr", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if result.Status != 0 {
		msg := fmt.Sprintf("Could not delete CTRL: %s", controller.Name)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	delete(s.Controllers, controller.Name)
	return &emptypb.Empty{}, nil
}

// UpdateNvmeController updates an Nvme controller
func (s *Server) UpdateNvmeController(_ context.Context, in *pb.UpdateNvmeControllerRequest) (*pb.NvmeController, error) {
	log.Printf("UpdateNvmeController: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.NvmeController.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	volume, ok := s.Controllers[in.NvmeController.Name]
	if !ok {
		if in.AllowMissing {
			log.Printf("TODO: in case of AllowMissing, create a new resource, don;t return error")
		}
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.NvmeController.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	resourceID := path.Base(volume.Name)
	// update_mask = 2
	if err := fieldmask.Validate(in.UpdateMask, in.NvmeController); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("TODO: use resourceID=%v", resourceID)
	subsys, ok := s.Subsystems[in.NvmeController.Spec.SubsystemId.Value]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.NvmeController.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}
	params := models.MrvlNvmSubsysCreateCtrlrParams{
		Subnqn:       subsys.Spec.Nqn,
		PcieDomainID: int(in.NvmeController.Spec.PcieId.PortId),
		PfID:         int(in.NvmeController.Spec.PcieId.PhysicalFunction),
		VfID:         int(in.NvmeController.Spec.PcieId.VirtualFunction),
		CtrlrID:      int(in.NvmeController.Spec.NvmeControllerId),
		MaxNsq:       int(in.NvmeController.Spec.MaxNsq),
		MaxNcq:       int(in.NvmeController.Spec.MaxNcq),
		Mqes:         int(in.NvmeController.Spec.Sqes),
	}
	var result models.MrvlNvmSubsysCreateCtrlrResult
	err := s.rpc.Call("mrvl_nvm_subsys_update_ctrlr", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if result.Status != 0 {
		msg := fmt.Sprintf("Could not update CTRL: %s", in.NvmeController.Name)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	response := server.ProtoClone(in.NvmeController)
	response.Spec.NvmeControllerId = int32(result.CtrlrID)
	response.Status = &pb.NvmeControllerStatus{Active: true}
	s.Controllers[in.NvmeController.Name] = response
	return response, nil
}

// ListNvmeControllers lists Nvme controllers
func (s *Server) ListNvmeControllers(_ context.Context, in *pb.ListNvmeControllersRequest) (*pb.ListNvmeControllersResponse, error) {
	log.Printf("ListNvmeControllers: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	size, offset, perr := server.ExtractPagination(in.PageSize, in.PageToken, s.Pagination)
	if perr != nil {
		log.Printf("error: %v", perr)
		return nil, perr
	}
	subsys, ok := s.Subsystems[in.Parent]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Parent)
		log.Printf("error: %v", err)
		return nil, err
	}
	params := models.MrvlNvmSubsysGetCtrlrListParams{
		Subnqn: subsys.Spec.Nqn,
	}
	var result models.MrvlNvmSubsysGetCtrlrListResult
	err := s.rpc.Call("mrvl_nvm_subsys_get_ctrlr_list", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if result.Status != 0 {
		msg := fmt.Sprintf("Could not list CTRLs: %v", in.Parent)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	token, hasMoreElements := "", false
	log.Printf("Limiting result len(%d) to [%d:%d]", len(result.CtrlrIDList), offset, size)
	result.CtrlrIDList, hasMoreElements = server.LimitPagination(result.CtrlrIDList, offset, size)
	if hasMoreElements {
		token = uuid.New().String()
		s.Pagination[token] = offset + size
	}
	Blobarray := make([]*pb.NvmeController, len(result.CtrlrIDList))
	for i := range result.CtrlrIDList {
		r := &result.CtrlrIDList[i]
		Blobarray[i] = &pb.NvmeController{Spec: &pb.NvmeControllerSpec{NvmeControllerId: int32(r.CtrlrID)}}
	}
	sortNvmeControllers(Blobarray)
	return &pb.ListNvmeControllersResponse{NvmeControllers: Blobarray}, nil
}

// GetNvmeController gets an Nvme controller
func (s *Server) GetNvmeController(_ context.Context, in *pb.GetNvmeControllerRequest) (*pb.NvmeController, error) {
	log.Printf("GetNvmeController: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	controller, ok := s.Controllers[in.Name]
	if !ok {
		return nil, fmt.Errorf("error finding controller %s", in.Name)
	}
	subsys, ok := s.Subsystems[controller.Spec.SubsystemId.Value]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", controller.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	params := models.MrvlNvmGetCtrlrInfoParams{
		Subnqn:  subsys.Spec.Nqn,
		CtrlrID: int(controller.Spec.NvmeControllerId),
	}
	var result models.MrvlNvmGetCtrlrInfoResult
	err := s.rpc.Call("mrvl_nvm_ctrlr_get_info", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if result.Status != 0 {
		msg := fmt.Sprintf("Could not get CTRL: %s", in.Name)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}

	return &pb.NvmeController{Name: in.Name, Spec: &pb.NvmeControllerSpec{NvmeControllerId: controller.Spec.NvmeControllerId}, Status: &pb.NvmeControllerStatus{Active: true}}, nil
}

// NvmeControllerStats gets an Nvme controller stats
func (s *Server) NvmeControllerStats(_ context.Context, in *pb.NvmeControllerStatsRequest) (*pb.NvmeControllerStatsResponse, error) {
	log.Printf("NvmeControllerStats: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Id.Value); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	controller, ok := s.Controllers[in.Id.Value]
	if !ok {
		return nil, fmt.Errorf("error finding controller %s", in.Id.Value)
	}
	subsys, ok := s.Subsystems[controller.Spec.SubsystemId.Value]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", controller.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	params := models.MrvlNvmGetCtrlrStatsParams{
		Subnqn:  subsys.Spec.Nqn,
		CtrlrID: int(controller.Spec.NvmeControllerId),
	}
	var result models.MrvlNvmGetCtrlrStatsResult
	err := s.rpc.Call("mrvl_nvm_get_ctrlr_stats", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if result.Status != 0 {
		msg := fmt.Sprintf("Could not stats CTRL: %s", in.Id.Value)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	return &pb.NvmeControllerStatsResponse{Stats: &pb.VolumeStats{
		ReadBytesCount:    int32(result.NumReadBytes),
		ReadOpsCount:      int32(result.NumReadCmds),
		WriteBytesCount:   int32(result.NumWriteBytes),
		WriteOpsCount:     int32(result.NumWriteCmds),
		ReadLatencyTicks:  int32(result.TotalReadLatencyInUs),
		WriteLatencyTicks: int32(result.TotalWriteLatencyInUs),
	}}, nil
}