// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package flightsql

import (
	"context"
	"errors"
	"io"

	"github.com/apache/arrow/go/v12/arrow"
	"github.com/apache/arrow/go/v12/arrow/array"
	"github.com/apache/arrow/go/v12/arrow/flight"
	pb "github.com/apache/arrow/go/v12/arrow/flight/internal/flight"
	"github.com/apache/arrow/go/v12/arrow/ipc"
	"github.com/apache/arrow/go/v12/arrow/memory"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// NewClient is a convenience function to automatically construct
// a flight.Client and return a flightsql.Client containing it rather
// than having to manually construct both yourself. It just delegates
// its arguments to flight.NewClientWithMiddleware to create the
// underlying Flight Client.
func NewClient(addr string, auth flight.ClientAuthHandler, middleware []flight.ClientMiddleware, opts ...grpc.DialOption) (*Client, error) {
	cl, err := flight.NewClientWithMiddleware(addr, auth, middleware, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{cl, memory.DefaultAllocator}, nil
}

// Client wraps a regular Flight RPC Client to provide the FlightSQL
// interface functions and methods.
type Client struct {
	Client flight.Client

	Alloc memory.Allocator
}

func descForCommand(cmd proto.Message) (*flight.FlightDescriptor, error) {
	var any anypb.Any
	if err := any.MarshalFrom(cmd); err != nil {
		return nil, err
	}

	data, err := proto.Marshal(&any)
	if err != nil {
		return nil, err
	}
	return &flight.FlightDescriptor{
		Type: flight.DescriptorCMD,
		Cmd:  data,
	}, nil
}

func flightInfoForCommand(ctx context.Context, cl *Client, cmd proto.Message, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	desc, err := descForCommand(cmd)
	if err != nil {
		return nil, err
	}
	return cl.getFlightInfo(ctx, desc, opts...)
}

func schemaForCommand(ctx context.Context, cl *Client, cmd proto.Message, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	desc, err := descForCommand(cmd)
	if err != nil {
		return nil, err
	}
	return cl.getSchema(ctx, desc, opts...)
}

// Execute executes the desired query on the server and returns a FlightInfo
// object describing where to retrieve the results.
func (c *Client) Execute(ctx context.Context, query string, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	cmd := pb.CommandStatementQuery{Query: query}
	return flightInfoForCommand(ctx, c, &cmd, opts...)
}

// GetExecuteSchema gets the schema of the result set of a query without
// executing the query itself.
func (c *Client) GetExecuteSchema(ctx context.Context, query string, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	cmd := pb.CommandStatementQuery{Query: query}
	return schemaForCommand(ctx, c, &cmd, opts...)
}

// ExecuteUpdate is for executing an update query and only returns the number of affected rows.
func (c *Client) ExecuteUpdate(ctx context.Context, query string, opts ...grpc.CallOption) (n int64, err error) {
	var (
		cmd          pb.CommandStatementUpdate
		desc         *flight.FlightDescriptor
		stream       pb.FlightService_DoPutClient
		res          *pb.PutResult
		updateResult pb.DoPutUpdateResult
	)

	cmd.Query = query
	if desc, err = descForCommand(&cmd); err != nil {
		return
	}

	if stream, err = c.Client.DoPut(ctx, opts...); err != nil {
		return
	}

	if err = stream.Send(&flight.FlightData{FlightDescriptor: desc}); err != nil {
		return
	}

	if err = stream.CloseSend(); err != nil {
		return
	}

	if res, err = stream.Recv(); err != nil {
		return
	}

	if err = proto.Unmarshal(res.GetAppMetadata(), &updateResult); err != nil {
		return
	}

	return updateResult.GetRecordCount(), nil
}

// GetCatalogs requests the list of catalogs from the server and
// returns a flightInfo object where the response can be retrieved
func (c *Client) GetCatalogs(ctx context.Context, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	return flightInfoForCommand(ctx, c, &pb.CommandGetCatalogs{}, opts...)
}

// GetCatalogsSchema requests the schema of GetCatalogs from the server
func (c *Client) GetCatalogsSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetCatalogs{}, opts...)
}

// GetDBSchemas requests the list of schemas from the database and
// returns a FlightInfo object where the response can be retrieved
func (c *Client) GetDBSchemas(ctx context.Context, cmdOpts *GetDBSchemasOpts, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	return flightInfoForCommand(ctx, c, (*pb.CommandGetDbSchemas)(cmdOpts), opts...)
}

// GetDBSchemasSchema requests the schema of GetDBSchemas from the server
func (c *Client) GetDBSchemasSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetDbSchemas{}, opts...)
}

// DoGet uses the provided flight ticket to request the stream of data.
// It returns a recordbatch reader to stream the results. Release
// should be called on the reader when done.
func (c *Client) DoGet(ctx context.Context, in *flight.Ticket, opts ...grpc.CallOption) (*flight.Reader, error) {
	stream, err := c.Client.DoGet(ctx, in, opts...)
	if err != nil {
		return nil, err
	}

	return flight.NewRecordReader(stream, ipc.WithAllocator(c.Alloc))
}

// GetTables requests a list of tables from the server, with the provided
// options describing how to make the request (filter patterns, if the schema
// should be returned, etc.). Returns a FlightInfo object where the response
// can be retrieved.
func (c *Client) GetTables(ctx context.Context, reqOptions *GetTablesOpts, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	return flightInfoForCommand(ctx, c, (*pb.CommandGetTables)(reqOptions), opts...)
}

// GetTablesSchema requests the schema of GetTables from the server.
func (c *Client) GetTablesSchema(ctx context.Context, reqOptions *GetTablesOpts, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, (*pb.CommandGetTables)(reqOptions), opts...)
}

// GetPrimaryKeys requests the primary keys for a specific table from the
// server, specified using a TableRef. Returns a FlightInfo object where
// the response can be retrieved.
func (c *Client) GetPrimaryKeys(ctx context.Context, ref TableRef, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	cmd := pb.CommandGetPrimaryKeys{
		Catalog:  ref.Catalog,
		DbSchema: ref.DBSchema,
		Table:    ref.Table,
	}
	return flightInfoForCommand(ctx, c, &cmd, opts...)
}

// GetPrimaryKeysSchema requests the schema of GetPrimaryKeys from the server.
func (c *Client) GetPrimaryKeysSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetPrimaryKeys{}, opts...)
}

// GetExportedKeys retrieves a description about the foreign key columns
// that reference the primary key columns of the specified table. Returns
// a FlightInfo object where the response can be retrieved.
func (c *Client) GetExportedKeys(ctx context.Context, ref TableRef, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	cmd := pb.CommandGetExportedKeys{
		Catalog:  ref.Catalog,
		DbSchema: ref.DBSchema,
		Table:    ref.Table,
	}
	return flightInfoForCommand(ctx, c, &cmd, opts...)
}

// GetExportedKeysSchema requests the schema of GetExportedKeys from the server.
func (c *Client) GetExportedKeysSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetExportedKeys{}, opts...)
}

// GetImportedKeys returns the foreign key columns for the specified table.
// Returns a FlightInfo object indicating where the response can be retrieved.
func (c *Client) GetImportedKeys(ctx context.Context, ref TableRef, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	cmd := pb.CommandGetImportedKeys{
		Catalog:  ref.Catalog,
		DbSchema: ref.DBSchema,
		Table:    ref.Table,
	}
	return flightInfoForCommand(ctx, c, &cmd, opts...)
}

// GetImportedKeysSchema requests the schema of GetImportedKeys from the server.
func (c *Client) GetImportedKeysSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetImportedKeys{}, opts...)
}

// GetCrossReference retrieves a description of the foreign key columns
// in the specified ForeignKey table that reference the primary key or
// columns representing a restraint of the parent table (could be the same
// or a different table). Returns a FlightInfo object indicating where
// the response can be retrieved with DoGet.
func (c *Client) GetCrossReference(ctx context.Context, pkTable, fkTable TableRef, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	cmd := pb.CommandGetCrossReference{
		PkCatalog:  pkTable.Catalog,
		PkDbSchema: pkTable.DBSchema,
		PkTable:    pkTable.Table,
		FkCatalog:  fkTable.Catalog,
		FkDbSchema: fkTable.DBSchema,
		FkTable:    fkTable.Table,
	}
	return flightInfoForCommand(ctx, c, &cmd, opts...)
}

// GetCrossReferenceSchema requests the schema of GetCrossReference from the server.
func (c *Client) GetCrossReferenceSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetCrossReference{}, opts...)
}

// GetTableTypes requests a list of the types of tables available on this
// server. Returns a FlightInfo object indicating where the response can
// be retrieved.
func (c *Client) GetTableTypes(ctx context.Context, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	return flightInfoForCommand(ctx, c, &pb.CommandGetTableTypes{}, opts...)
}

// GetTableTypesSchema requests the schema of GetTableTypes from the server.
func (c *Client) GetTableTypesSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetTableTypes{}, opts...)
}

// GetXdbcTypeInfo requests the information about all the data types supported
// (dataType == nil) or a specific data type. Returns a FlightInfo object
// indicating where the response can be retrieved.
func (c *Client) GetXdbcTypeInfo(ctx context.Context, dataType *int32, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	return flightInfoForCommand(ctx, c, &pb.CommandGetXdbcTypeInfo{DataType: dataType}, opts...)
}

// GetXdbcTypeInfoSchema requests the schema of GetXdbcTypeInfo from the server.
func (c *Client) GetXdbcTypeInfoSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetXdbcTypeInfo{}, opts...)
}

// GetSqlInfo returns a list of the requested SQL information corresponding
// to the values in the info slice. Returns a FlightInfo object indicating
// where the response can be retrieved.
func (c *Client) GetSqlInfo(ctx context.Context, info []SqlInfo, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	cmd := &pb.CommandGetSqlInfo{Info: make([]uint32, len(info))}

	for i, v := range info {
		cmd.Info[i] = uint32(v)
	}
	return flightInfoForCommand(ctx, c, cmd, opts...)
}

// GetSqlInfoSchema requests the schema of  GetSqlInfo from the server.
func (c *Client) GetSqlInfoSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return schemaForCommand(ctx, c, &pb.CommandGetSqlInfo{}, opts...)
}

// Prepare creates a PreparedStatement object for the specified query.
// The resulting PreparedStatement object should be Closed when no longer
// needed. It will maintain a reference to this Client for use to execute
// and use the specified allocator for any allocations it needs to perform.
func (c *Client) Prepare(ctx context.Context, mem memory.Allocator, query string, opts ...grpc.CallOption) (prep *PreparedStatement, err error) {
	const actionType = CreatePreparedStatementActionType

	var (
		cmd, cmdResult        anypb.Any
		res                   *pb.Result
		request               pb.ActionCreatePreparedStatementRequest
		result                pb.ActionCreatePreparedStatementResult
		action                pb.Action
		stream                pb.FlightService_DoActionClient
		dsSchema, paramSchema *arrow.Schema
	)

	request.Query = query
	if err = cmd.MarshalFrom(&request); err != nil {
		return
	}

	action.Type = actionType
	if action.Body, err = proto.Marshal(&cmd); err != nil {
		return
	}

	if stream, err = c.Client.DoAction(ctx, &action, opts...); err != nil {
		return
	}

	if res, err = stream.Recv(); err != nil {
		return
	}

	if err = proto.Unmarshal(res.Body, &cmdResult); err != nil {
		return
	}

	if err = cmdResult.UnmarshalTo(&result); err != nil {
		return
	}

	if result.DatasetSchema != nil {
		dsSchema, err = flight.DeserializeSchema(result.DatasetSchema, mem)
		if err != nil {
			return
		}
	}
	if result.ParameterSchema != nil {
		paramSchema, err = flight.DeserializeSchema(result.ParameterSchema, mem)
		if err != nil {
			return
		}
	}

	prep = &PreparedStatement{
		client:        c,
		handle:        result.PreparedStatementHandle,
		datasetSchema: dsSchema,
		paramSchema:   paramSchema,
	}
	return
}

func (c *Client) getFlightInfo(ctx context.Context, desc *flight.FlightDescriptor, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	return c.Client.GetFlightInfo(ctx, desc, opts...)
}

func (c *Client) getSchema(ctx context.Context, desc *flight.FlightDescriptor, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	return c.Client.GetSchema(ctx, desc, opts...)
}

// Close will close the underlying flight Client in use by this flightsql.Client
func (c *Client) Close() error { return c.Client.Close() }

// PreparedStatement represents a constructed PreparedStatement on the server
// and maintains a reference to the Client that created it along with the
// prepared statement handle.
//
// If the server returned the Dataset Schema or Parameter Binding schemas
// at creation, they will also be accessible from this object. Close
// should be called when no longer needed.
type PreparedStatement struct {
	client        *Client
	handle        []byte
	datasetSchema *arrow.Schema
	paramSchema   *arrow.Schema
	paramBinding  arrow.Record
	streamBinding array.RecordReader
	closed        bool
}

// Execute executes the prepared statement on the server and returns a FlightInfo
// indicating where to retrieve the response. If SetParameters has been called
// then the parameter bindings will be sent before execution.
//
// Will error if already closed.
func (p *PreparedStatement) Execute(ctx context.Context, opts ...grpc.CallOption) (*flight.FlightInfo, error) {
	if p.closed {
		return nil, errors.New("arrow/flightsql: prepared statement already closed")
	}

	cmd := &pb.CommandPreparedStatementQuery{PreparedStatementHandle: p.handle}

	desc, err := descForCommand(cmd)
	if err != nil {
		return nil, err
	}

	if p.hasBindParameters() {
		pstream, err := p.client.Client.DoPut(ctx, opts...)
		if err != nil {
			return nil, err
		}

		wr, err := p.writeBindParameters(pstream, desc)
		if err != nil {
			return nil, err
		}
		if err = wr.Close(); err != nil {
			return nil, err
		}
		pstream.CloseSend()

		// wait for the server to ack the result
		if _, err = pstream.Recv(); err != nil && err != io.EOF {
			return nil, err
		}
	}

	return p.client.getFlightInfo(ctx, desc, opts...)
}

// ExecuteUpdate executes the prepared statement update query on the server
// and returns the number of rows affected. If SetParameters was called,
// the parameter bindings will be sent with the request to execute.
func (p *PreparedStatement) ExecuteUpdate(ctx context.Context, opts ...grpc.CallOption) (nrecords int64, err error) {
	if p.closed {
		return 0, errors.New("arrow/flightsql: prepared statement already closed")
	}

	var (
		execCmd      = &pb.CommandPreparedStatementUpdate{PreparedStatementHandle: p.handle}
		desc         *flight.FlightDescriptor
		pstream      pb.FlightService_DoPutClient
		wr           *flight.Writer
		res          *pb.PutResult
		updateResult pb.DoPutUpdateResult
	)

	desc, err = descForCommand(execCmd)
	if err != nil {
		return
	}

	if pstream, err = p.client.Client.DoPut(ctx, opts...); err != nil {
		return
	}
	if p.hasBindParameters() {
		wr, err = p.writeBindParameters(pstream, desc)
		if err != nil {
			return
		}
	} else {
		schema := arrow.NewSchema([]arrow.Field{}, nil)
		wr = flight.NewRecordWriter(pstream, ipc.WithSchema(schema))
		wr.SetFlightDescriptor(desc)
		rec := array.NewRecord(schema, []arrow.Array{}, 0)
		if err = wr.Write(rec); err != nil {
			return
		}
	}

	if err = wr.Close(); err != nil {
		return
	}
	if err = pstream.CloseSend(); err != nil {
		return
	}
	if res, err = pstream.Recv(); err != nil {
		return
	}

	if err = proto.Unmarshal(res.GetAppMetadata(), &updateResult); err != nil {
		return
	}

	return updateResult.GetRecordCount(), nil
}

func (p *PreparedStatement) hasBindParameters() bool {
	return (p.paramBinding != nil && p.paramBinding.NumRows() > 0) || (p.streamBinding != nil)
}

func (p *PreparedStatement) writeBindParameters(pstream pb.FlightService_DoPutClient, desc *pb.FlightDescriptor) (*flight.Writer, error) {
	if p.paramBinding != nil {
		wr := flight.NewRecordWriter(pstream, ipc.WithSchema(p.paramBinding.Schema()))
		wr.SetFlightDescriptor(desc)
		if err := wr.Write(p.paramBinding); err != nil {
			return nil, err
		}
		return wr, nil
	} else {
		wr := flight.NewRecordWriter(pstream, ipc.WithSchema(p.streamBinding.Schema()))
		wr.SetFlightDescriptor(desc)
		for p.streamBinding.Next() {
			if err := wr.Write(p.streamBinding.Record()); err != nil {
				return nil, err
			}
		}
		if err := p.streamBinding.Err(); err != nil {
			return nil, err
		}
		return wr, nil
	}
}

// DatasetSchema may be nil if the server did not return it when creating the
// Prepared Statement.
func (p *PreparedStatement) DatasetSchema() *arrow.Schema { return p.datasetSchema }

// ParameterSchema may be nil if the server did not return it when creating
// the prepared statement.
func (p *PreparedStatement) ParameterSchema() *arrow.Schema { return p.paramSchema }

// GetSchema re-requests the schema of the result set of the prepared
// statement from the server. It should otherwise be identical to DatasetSchema.
//
// Will error if already closed.
func (p *PreparedStatement) GetSchema(ctx context.Context, opts ...grpc.CallOption) (*flight.SchemaResult, error) {
	if p.closed {
		return nil, errors.New("arrow/flightsql: prepared statement already closed")
	}

	cmd := &pb.CommandPreparedStatementQuery{PreparedStatementHandle: p.handle}

	desc, err := descForCommand(cmd)
	if err != nil {
		return nil, err
	}

	return p.client.getSchema(ctx, desc, opts...)
}

func (p *PreparedStatement) clearParameters() {
	if p.paramBinding != nil {
		p.paramBinding.Release()
		p.paramBinding = nil
	}
	if p.streamBinding != nil {
		p.streamBinding.Release()
		p.streamBinding = nil
	}
}

// SetParameters takes a record batch to send as the parameter bindings when
// executing. It should match the schema from ParameterSchema.
//
// This will call Retain on the record to ensure it doesn't get released out
// from under the statement. Release will be called on a previous binding
// record or reader if it existed, and will be called upon calling Close on the
// PreparedStatement.
func (p *PreparedStatement) SetParameters(binding arrow.Record) {
	p.clearParameters()
	p.paramBinding = binding
	p.paramBinding.Retain()
}

// SetRecordReader takes a RecordReader to send as the parameter bindings when
// executing. It should match the schema from ParameterSchema.
//
// This will call Retain on the reader to ensure it doesn't get released out
// from under the statement. Release will be called on a previous binding
// record or reader if it existed, and will be called upon calling Close on the
// PreparedStatement.
func (p *PreparedStatement) SetRecordReader(binding array.RecordReader) {
	p.clearParameters()
	binding.Retain()
	p.streamBinding = binding
	p.streamBinding.Retain()
}

// Close calls release on any parameter binding record and sends
// a ClosePreparedStatement action to the server. After calling
// Close, the PreparedStatement should not be used again.
func (p *PreparedStatement) Close(ctx context.Context, opts ...grpc.CallOption) error {
	if p.closed {
		return errors.New("arrow/flightsql: already closed")
	}

	p.clearParameters()

	const actionType = ClosePreparedStatementActionType
	var (
		cmd     anypb.Any
		request pb.ActionClosePreparedStatementRequest
	)

	request.PreparedStatementHandle = p.handle
	if err := cmd.MarshalFrom(&request); err != nil {
		return err
	}

	body, err := proto.Marshal(&cmd)
	if err != nil {
		return err
	}

	action := &flight.Action{Type: actionType, Body: body}
	_, err = p.client.Client.DoAction(ctx, action, opts...)
	if err != nil {
		return err
	}

	p.closed = true
	return nil
}
