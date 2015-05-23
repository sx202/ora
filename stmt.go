// Copyright 2014 Rana Ian. All rights reserved.
// Use of this source code is governed by The MIT License
// found in the accompanying LICENSE file.

package ora

/*
#include <oci.h>
#include <stdlib.h>
*/
import "C"
import (
	"container/list"
	"reflect"
	"strings"
	"time"
	"unsafe"
)

// Stmt is an Oracle statement associated with a session.
type Stmt struct {
	id      uint64
	ses     *Ses
	ocistmt *C.OCIStmt

	rsetId     uint64
	rsets      *list.List
	elem       *list.Element
	Cfg        StmtCfg
	bnds       []bnd
	gcts       []GoColumnType
	sql        string
	stmtType   C.ub4
	hasPtrBind bool
}

// NumRset returns the number of open Oracle result sets.
func (stmt *Stmt) NumRset() int {
	return stmt.rsets.Len()
}

// NumInput returns the number of placeholders in a sql statement.
func (stmt *Stmt) NumInput() int {
	var bindCount uint32
	if err := stmt.attr(unsafe.Pointer(&bindCount), 4, C.OCI_ATTR_BIND_COUNT); err != nil {
		return 0
	}
	return int(bindCount)
}

// checkIsOpen validates that the statement is open.
func (stmt *Stmt) checkIsOpen() error {
	if stmt == nil {
		return errNew("Stmt is not initialized")
	}
	if !stmt.IsOpen() {
		return errNewF("Stmt is closed (id %v)", stmt.id)
	}
	return nil
}

// IsOpen returns true when a statement is open; otherwise, false.
//
// Calling Close will cause Stmt.IsOpen to return false. Once closed, a statement
// cannot be re-opened. Call Stmt.Prep to create a new statement.
func (stmt *Stmt) IsOpen() bool {
	return stmt.ses != nil
}

// Close closes the SQL statement.
//
// Calling Close will cause Stmt.IsOpen to return false. Once closed, a statement
// cannot be re-opened. Call Stmt.Prep to create a new statement.
func (stmt *Stmt) Close() (err error) {
	if err := stmt.checkIsOpen(); err != nil {
		return err
	}
	Log.Infof("E%vS%vS%vS%v] Close", stmt.ses.srv.env.id, stmt.ses.srv.id, stmt.ses.id, stmt.id)
	errs := stmt.ses.srv.env.drv.listPool.Get().(*list.List)
	defer func() {
		if value := recover(); value != nil {
			Log.Errorln(recoverMsg(value))
			errs.PushBack(errRecover(value))
		}

		// free ocistmt to release cursor on server
		stmt.ses.srv.env.freeOciHandle(unsafe.Pointer(stmt.ocistmt), C.OCI_HTYPE_STMT)

		ses := stmt.ses
		ses.stmts.Remove(stmt.elem)
		stmt.rsets.Init()
		stmt.ses = nil
		stmt.ocistmt = nil
		stmt.elem = nil
		stmt.bnds = nil
		stmt.gcts = nil
		stmt.sql = ""
		stmt.stmtType = C.ub4(0)
		stmt.hasPtrBind = false
		ses.srv.env.drv.stmtPool.Put(stmt)

		m := newMultiErrL(errs)
		if m != nil {
			err = *m
		}
		errs.Init()
		ses.srv.env.drv.listPool.Put(errs)
	}()

	// close binds
	if len(stmt.bnds) > 0 {
		for _, bind := range stmt.bnds {
			if bind != nil {
				err0 := bind.close()
				errs.PushBack(err0)
			}
		}
	}
	// close result sets
	for e := stmt.rsets.Front(); e != nil; e = e.Next() {
		err0 := e.Value.(*Rset).close()
		errs.PushBack(err0)
	}

	return err
}

// Exe executes a SQL statement on an Oracle server returning the number of
// rows affected and a possible error.
func (stmt *Stmt) Exe(params ...interface{}) (rowsAffected uint64, err error) {
	rowsAffected, _, err = stmt.exe(params)
	return rowsAffected, err
}

// exe executes a SQL statement on an Oracle server returning rowsAffected, lastInsertId and error.
func (stmt *Stmt) exe(params []interface{}) (rowsAffected uint64, lastInsertId int64, err error) {
	if err := stmt.checkIsOpen(); err != nil {
		return 0, 0, err
	}
	// for case of inserting and returning identity for database/sql package
	if stmt.ses.srv.env.isSqlPkg && stmt.stmtType == C.OCI_STMT_INSERT {
		lastIndex := strings.LastIndex(stmt.sql, ")")
		sqlEnd := stmt.sql[lastIndex+1 : len(stmt.sql)]
		sqlEnd = strings.ToUpper(sqlEnd)
		// add *int64 arg to capture identity
		if strings.Contains(sqlEnd, "RETURNING") {
			params[len(params)-1] = &lastInsertId
		}
	}
	// bind parameters
	iterations, err := stmt.bind(params)
	if err != nil {
		return 0, 0, err
	}
	// set prefetch size
	err = stmt.setPrefetchSize()
	if err != nil {
		return 0, 0, err
	}
	// determine auto-commit state
	// don't auto comit if there is a transaction occuring
	var mode C.ub4
	if stmt.Cfg.IsAutoCommitting && stmt.ses.txs.Front() == nil {
		mode = C.OCI_COMMIT_ON_SUCCESS
	} else {
		mode = C.OCI_DEFAULT
	}
	// Execute statement on Oracle server
	r := C.OCIStmtExecute(
		stmt.ses.srv.ocisvcctx,  //OCISvcCtx           *svchp,
		stmt.ocistmt,            //OCIStmt             *stmtp,
		stmt.ses.srv.env.ocierr, //OCIError            *errhp,
		C.ub4(iterations),       //ub4                 iters,
		C.ub4(0),                //ub4                 rowoff,
		nil,                     //const OCISnapshot   *snap_in,
		nil,                     //OCISnapshot         *snap_out,
		mode)                    //ub4                 mode );
	if r == C.OCI_ERROR {
		return 0, 0, stmt.ses.srv.env.ociError()
	}

	// Get row count based on statement type
	var rowCount C.ub8
	switch stmt.stmtType {
	case C.OCI_STMT_SELECT, C.OCI_STMT_UPDATE, C.OCI_STMT_DELETE, C.OCI_STMT_INSERT:
		err := stmt.attr(unsafe.Pointer(&rowCount), 8, C.OCI_ATTR_UB8_ROW_COUNT)
		if err != nil {
			return 0, 0, err
		}
		rowsAffected = uint64(rowCount)
	case C.OCI_STMT_CREATE, C.OCI_STMT_DROP, C.OCI_STMT_ALTER, C.OCI_STMT_BEGIN:
	}

	// Set any bind pointers
	if stmt.hasPtrBind {
		err = stmt.setBindPtrs()
		if err != nil {
			return rowsAffected, lastInsertId, err
		}
	}

	return rowsAffected, lastInsertId, nil
}

// Qry runs a SQL query on an Oracle server returning a *Rset and possible error.
func (stmt *Stmt) Qry(params ...interface{}) (*Rset, error) {
	return stmt.qry(params)
}

// qry runs a SQL query on an Oracle server returning a *Rset and possible error.
func (stmt *Stmt) qry(params []interface{}) (*Rset, error) {
	if err := stmt.checkIsOpen(); err != nil {
		return nil, err
	}
	// bind parameters
	_, err := stmt.bind(params)
	if err != nil {
		return nil, err
	}
	// set prefetch size
	err = stmt.setPrefetchSize()
	if err != nil {
		return nil, err
	}
	// run query
	r := C.OCIStmtExecute(
		stmt.ses.srv.ocisvcctx,  //OCISvcCtx           *svchp,
		stmt.ocistmt,            //OCIStmt             *stmtp,
		stmt.ses.srv.env.ocierr, //OCIError            *errhp,
		C.ub4(0),                //ub4                 iters,
		C.ub4(0),                //ub4                 rowoff,
		nil,                     //const OCISnapshot   *snap_in,
		nil,                     //OCISnapshot         *snap_out,
		C.OCI_DEFAULT)           //ub4                 mode );
	if r == C.OCI_ERROR {
		return nil, stmt.ses.srv.env.ociError()
	}
	// set any bind pointers
	if stmt.hasPtrBind {
		err = stmt.setBindPtrs()
		if err != nil {
			return nil, err
		}
	}
	// create result set and open
	rset := &Rset{}
	err = rset.open(stmt, stmt.ocistmt)
	if err != nil {
		rset.close()
		return nil, err
	}
	// store result set for later close call
	stmt.rsets.PushBack(rset)
	return rset, nil
}

// setBindPtrs enables binds to set out pointers for some types such as time.Time, etc.
func (stmt *Stmt) setBindPtrs() (err error) {
	for _, bind := range stmt.bnds {
		err = bind.setPtr()
		if err != nil {
			return err
		}
	}
	return nil
}

// gets a bind struct from a driver slice
func (stmt *Stmt) getBnd(idx int) interface{} {
	return stmt.ses.srv.env.drv.bndPools[idx].Get()
}

// puts a bind struct in the driver slice
func (stmt *Stmt) putBnd(idx int, bnd bnd) {
	stmt.ses.srv.env.drv.bndPools[idx].Put(bnd)
}

// bind associates Go variables to SQL string placeholders by the
// position of the variable and the position of the placeholder.
//
// The first placeholder starts at position 1.
//
// The placeholder represents an input bind when the value is a built-in value type
// or an array or slice of builtin value types. The placeholder represents an
// output bind when the value is a pointer to a built-in value type
// or an array or slice of pointers to builtin value types.
func (stmt *Stmt) bind(params []interface{}) (iterations uint32, err error) {
	//fmt.Printf("Stmt.bind: len(params) (%v)\n", len(params))

	iterations = 1
	// Create binds for each parameter; bind position is 1-based
	if params != nil && len(params) > 0 {
		stmt.bnds = make([]bnd, len(params))
		for n := range params {
			//fmt.Printf("Stmt.bind: params[%v] (%v)\n", n, params[n])
			if value, ok := params[n].(int64); ok {
				bnd := stmt.getBnd(bndIdxInt64).(*bndInt64)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(int32); ok {
				bnd := stmt.getBnd(bndIdxInt32).(*bndInt32)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(int16); ok {
				bnd := stmt.getBnd(bndIdxInt16).(*bndInt16)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(int8); ok {
				bnd := stmt.getBnd(bndIdxInt8).(*bndInt8)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(uint64); ok {
				bnd := stmt.getBnd(bndIdxUint64).(*bndUint64)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(uint32); ok {
				bnd := stmt.getBnd(bndIdxUint32).(*bndUint32)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(uint16); ok {
				bnd := stmt.getBnd(bndIdxUint16).(*bndUint16)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(uint8); ok {
				bnd := stmt.getBnd(bndIdxUint8).(*bndUint8)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(float64); ok {
				bnd := stmt.getBnd(bndIdxFloat64).(*bndFloat64)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(float32); ok {
				bnd := stmt.getBnd(bndIdxFloat32).(*bndFloat32)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(Int64); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_INT)
				} else {
					bnd := stmt.getBnd(bndIdxInt64).(*bndInt64)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Int32); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_INT)
				} else {
					bnd := stmt.getBnd(bndIdxInt32).(*bndInt32)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Int16); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_INT)
				} else {
					bnd := stmt.getBnd(bndIdxInt16).(*bndInt16)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Int8); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_INT)
				} else {
					bnd := stmt.getBnd(bndIdxInt8).(*bndInt8)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Uint64); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_UIN)
				} else {
					bnd := stmt.getBnd(bndIdxUint64).(*bndUint64)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Uint32); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_UIN)
				} else {
					bnd := stmt.getBnd(bndIdxUint32).(*bndUint32)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Uint16); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_UIN)
				} else {
					bnd := stmt.getBnd(bndIdxUint16).(*bndUint16)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Uint8); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_UIN)
				} else {
					bnd := stmt.getBnd(bndIdxUint8).(*bndUint8)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Float64); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_BDOUBLE)
				} else {
					bnd := stmt.getBnd(bndIdxFloat64).(*bndFloat64)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(Float32); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_BFLOAT)
				} else {
					bnd := stmt.getBnd(bndIdxFloat32).(*bndFloat32)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(*int64); ok {
				bnd := stmt.getBnd(bndIdxInt64Ptr).(*bndInt64Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*int32); ok {
				bnd := stmt.getBnd(bndIdxInt32Ptr).(*bndInt32Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*int16); ok {
				bnd := stmt.getBnd(bndIdxInt16Ptr).(*bndInt16Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*int8); ok {
				bnd := stmt.getBnd(bndIdxInt8Ptr).(*bndInt8Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*uint64); ok {
				bnd := stmt.getBnd(bndIdxUint64Ptr).(*bndUint64Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*uint32); ok {
				bnd := stmt.getBnd(bndIdxUint32Ptr).(*bndUint32Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*uint16); ok {
				bnd := stmt.getBnd(bndIdxUint16Ptr).(*bndUint16Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*uint8); ok {
				bnd := stmt.getBnd(bndIdxUint8Ptr).(*bndUint8Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*float64); ok {
				bnd := stmt.getBnd(bndIdxFloat64Ptr).(*bndFloat64Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(*float32); ok {
				bnd := stmt.getBnd(bndIdxFloat32Ptr).(*bndFloat32Ptr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].([]int64); ok {
				bnd := stmt.getBnd(bndIdxInt64Slice).(*bndInt64Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]int32); ok {
				bnd := stmt.getBnd(bndIdxInt32Slice).(*bndInt32Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]int16); ok {
				bnd := stmt.getBnd(bndIdxInt16Slice).(*bndInt16Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]int8); ok {
				bnd := stmt.getBnd(bndIdxInt8Slice).(*bndInt8Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]uint64); ok {
				bnd := stmt.getBnd(bndIdxUint64Slice).(*bndUint64Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]uint32); ok {
				bnd := stmt.getBnd(bndIdxUint32Slice).(*bndUint32Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]uint16); ok {
				bnd := stmt.getBnd(bndIdxUint16Slice).(*bndUint16Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]uint8); ok {
				if stmt.Cfg.byteSlice == U8 {
					bnd := stmt.getBnd(bndIdxUint8Slice).(*bndUint8Slice)
					stmt.bnds[n] = bnd
					err = bnd.bind(value, nil, n+1, stmt)
					if err != nil {
						return iterations, err
					}
					iterations = uint32(len(value))
				} else {
					bnd := stmt.getBnd(bndIdxBin).(*bndBin)
					stmt.bnds[n] = bnd
					err = bnd.bind(value, n+1, stmt.Cfg.lobBufferSize, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].([]float64); ok {
				bnd := stmt.getBnd(bndIdxFloat64Slice).(*bndFloat64Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]float32); ok {
				bnd := stmt.getBnd(bndIdxFloat32Slice).(*bndFloat32Slice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))

			} else if value, ok := params[n].([]Int64); ok {
				bnd := stmt.getBnd(bndIdxInt64Slice).(*bndInt64Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Int32); ok {
				bnd := stmt.getBnd(bndIdxInt32Slice).(*bndInt32Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Int16); ok {
				bnd := stmt.getBnd(bndIdxInt16Slice).(*bndInt16Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Int8); ok {
				bnd := stmt.getBnd(bndIdxInt8Slice).(*bndInt8Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Uint64); ok {
				bnd := stmt.getBnd(bndIdxUint64Slice).(*bndUint64Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Uint32); ok {
				bnd := stmt.getBnd(bndIdxUint32Slice).(*bndUint32Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Uint16); ok {
				bnd := stmt.getBnd(bndIdxUint16Slice).(*bndUint16Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Uint8); ok {
				bnd := stmt.getBnd(bndIdxUint8Slice).(*bndUint8Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Float64); ok {
				bnd := stmt.getBnd(bndIdxFloat64Slice).(*bndFloat64Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Float32); ok {
				bnd := stmt.getBnd(bndIdxFloat32Slice).(*bndFloat32Slice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].(time.Time); ok {
				bnd := stmt.getBnd(bndIdxTime).(*bndTime)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(*time.Time); ok {
				bnd := stmt.getBnd(bndIdxTimePtr).(*bndTimePtr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(Time); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_TIMESTAMP_TZ)
				} else {
					bnd := stmt.getBnd(bndIdxTime).(*bndTime)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].([]time.Time); ok {
				bnd := stmt.getBnd(bndIdxTimeSlice).(*bndTimeSlice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Time); ok {
				bnd := stmt.getBnd(bndIdxTimeSlice).(*bndTimeSlice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].(string); ok {
				bnd := stmt.getBnd(bndIdxString).(*bndString)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(*string); ok {
				bnd := stmt.getBnd(bndIdxStringPtr).(*bndStringPtr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt.Cfg.stringPtrBufferSize, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(String); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_CHR)
				} else {
					bnd := stmt.getBnd(bndIdxString).(*bndString)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].([]string); ok {
				bnd := stmt.getBnd(bndIdxStringSlice).(*bndStringSlice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]String); ok {
				bnd := stmt.getBnd(bndIdxStringSlice).(*bndStringSlice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].(bool); ok {
				bnd := stmt.getBnd(bndIdxBool).(*bndBool)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt.Cfg, stmt)
				if err != nil {
					return iterations, err
				}
			} else if value, ok := params[n].(*bool); ok {
				bnd := stmt.getBnd(bndIdxBoolPtr).(*bndBoolPtr)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt.Cfg.TrueRune, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if value, ok := params[n].(Bool); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_CHR)
				} else {
					bnd := stmt.getBnd(bndIdxBool).(*bndBool)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt.Cfg, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].([]bool); ok {
				bnd := stmt.getBnd(bndIdxBoolSlice).(*bndBoolSlice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt.Cfg.FalseRune, stmt.Cfg.TrueRune, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Bool); ok {
				bnd := stmt.getBnd(bndIdxBoolSlice).(*bndBoolSlice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt.Cfg.FalseRune, stmt.Cfg.TrueRune, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].(Binary); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_BLOB)
				} else {
					bnd := stmt.getBnd(bndIdxBin).(*bndBin)
					stmt.bnds[n] = bnd
					err = bnd.bind(value.Value, n+1, stmt.Cfg.lobBufferSize, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].([][]byte); ok {
				bnd := stmt.getBnd(bndIdxBinSlice).(*bndBinSlice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, nil, n+1, stmt.Cfg.lobBufferSize, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].([]Binary); ok {
				bnd := stmt.getBnd(bndIdxBinSlice).(*bndBinSlice)
				stmt.bnds[n] = bnd
				err = bnd.bindOra(value, n+1, stmt.Cfg.lobBufferSize, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].(IntervalYM); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_INTERVAL_YM)
				} else {
					bnd := stmt.getBnd(bndIdxIntervalYM).(*bndIntervalYM)
					stmt.bnds[n] = bnd
					err = bnd.bind(value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].([]IntervalYM); ok {
				bnd := stmt.getBnd(bndIdxIntervalYMSlice).(*bndIntervalYMSlice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].(IntervalDS); ok {
				if value.IsNull {
					stmt.setNilBind(n, C.SQLT_INTERVAL_DS)
				} else {
					bnd := stmt.getBnd(bndIdxIntervalDS).(*bndIntervalDS)
					stmt.bnds[n] = bnd
					err = bnd.bind(value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].([]IntervalDS); ok {
				bnd := stmt.getBnd(bndIdxIntervalDSSlice).(*bndIntervalDSSlice)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				iterations = uint32(len(value))
			} else if value, ok := params[n].(Bfile); ok {
				if value.IsNull {
					err = stmt.setNilBind(n, C.SQLT_FILE)
				} else {
					bnd := stmt.getBnd(bndIdxBfile).(*bndBfile)
					stmt.bnds[n] = bnd
					err = bnd.bind(value, n+1, stmt)
					if err != nil {
						return iterations, err
					}
				}
			} else if value, ok := params[n].(*Rset); ok {
				bnd := stmt.getBnd(bndIdxRset).(*bndRset)
				stmt.bnds[n] = bnd
				err = bnd.bind(value, n+1, stmt)
				if err != nil {
					return iterations, err
				}
				stmt.hasPtrBind = true
			} else if params[n] == nil {
				err = stmt.setNilBind(n, C.SQLT_CHR)
			} else {
				t := reflect.TypeOf(params[n])
				if t.Kind() == reflect.Slice {
					if t.Elem().Kind() == reflect.Interface {
						return iterations, errNewF("Invalid bind parameter. ([]interface{}) (%v).", params[n])
					}
				}
				return iterations, errNewF("Invalid bind parameter (%v) (%v).", t.Name(), params[n])
			}
		}
	}

	return iterations, err
}

// setNilBind sets a nil bind.
func (stmt *Stmt) setNilBind(index int, sqlt C.ub2) (err error) {
	bnd := stmt.ses.srv.env.drv.bndPools[bndIdxNil].Get().(*bndNil)
	stmt.bnds[index] = bnd
	err = bnd.bind(index+1, sqlt, stmt)
	return err
}

// set prefetch size
func (stmt *Stmt) setPrefetchSize() error {
	if stmt.Cfg.prefetchRowCount > 0 {
		//fmt.Println("stmt.setPrefetchSize: prefetchRowCount ", stmt.Cfg.prefetchRowCount)
		// set prefetch row count
		if err := stmt.setAttr(unsafe.Pointer(&stmt.Cfg.prefetchRowCount), 4, C.OCI_ATTR_PREFETCH_ROWS); err != nil {
			return err
		}
	} else {
		//fmt.Println("stmt.setPrefetchSize: prefetchMemorySize ", stmt.Cfg.prefetchMemorySize)
		// Set prefetch memory size
		if err := stmt.setAttr(unsafe.Pointer(&stmt.Cfg.prefetchMemorySize), 4, C.OCI_ATTR_PREFETCH_MEMORY); err != nil {
			return err
		}
	}
	return nil
}

// attr gets an attribute from the statement handle.
func (stmt *Stmt) attr(attrup unsafe.Pointer, attrSize C.ub4, attrType C.ub4) error {
	r := C.OCIAttrGet(
		unsafe.Pointer(stmt.ocistmt), //const void     *trgthndlp,
		C.OCI_HTYPE_STMT,             //ub4            trghndltyp,
		attrup,                       //void           *attributep,
		&attrSize,                    //ub4            *sizep,
		attrType,                     //ub4            attrtype,
		stmt.ses.srv.env.ocierr)      //OCIError       *errhp );
	if r == C.OCI_ERROR {
		return stmt.ses.srv.env.ociError()
	}
	return nil
}

// setAttr sets an attribute on the statement handle.
func (stmt *Stmt) setAttr(attrup unsafe.Pointer, attrSize C.ub4, attrType C.ub4) error {
	r := C.OCIAttrSet(
		unsafe.Pointer(stmt.ocistmt), //void        *trgthndlp,
		C.OCI_HTYPE_STMT,             //ub4         trghndltyp,
		attrup,                       //void        *attributep,
		attrSize,                     //ub4         size,
		attrType,                     //ub4         attrtype,
		stmt.ses.srv.env.ocierr)      //OCIError    *errhp );
	if r == C.OCI_ERROR {
		return stmt.ses.srv.env.ociError()
	}

	return nil
}