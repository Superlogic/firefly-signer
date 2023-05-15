// Code generated by mockery v2.26.1. DO NOT EDIT.

package rpcservermocks

import mock "github.com/stretchr/testify/mock"

// Server is an autogenerated mock type for the Server type
type Server struct {
	mock.Mock
}

// Start provides a mock function with given fields:
func (_m *Server) Start() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Stop provides a mock function with given fields:
func (_m *Server) Stop() {
	_m.Called()
}

// WaitStop provides a mock function with given fields:
func (_m *Server) WaitStop() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

type mockConstructorTestingTNewServer interface {
	mock.TestingT
	Cleanup(func())
}

// NewServer creates a new instance of Server. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func NewServer(t mockConstructorTestingTNewServer) *Server {
	mock := &Server{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
