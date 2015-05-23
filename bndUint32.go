// Copyright 2014 Rana Ian. All rights reserved.
// Use of this source code is governed by The MIT License
// found in the accompanying LICENSE file.

package ora

/*
#include <oci.h>
*/
import "C"
import (
	"unsafe"
)

type bndUint32 struct {
	stmt      *Stmt
	ocibnd    *C.OCIBind
	ociNumber C.OCINumber
}

func (bnd *bndUint32) bind(value uint32, position int, stmt *Stmt) error {
	bnd.stmt = stmt
	r := C.OCINumberFromInt(
		bnd.stmt.ses.srv.env.ocierr, //OCIError            *err,
		unsafe.Pointer(&value),      //const void          *inum,
		4, //uword               inum_length,
		C.OCI_NUMBER_UNSIGNED, //uword               inum_s_flag,
		&bnd.ociNumber)        //OCINumber           *number );
	if r == C.OCI_ERROR {
		return bnd.stmt.ses.srv.env.ociError()
	}
	r = C.OCIBindByPos2(
		bnd.stmt.ocistmt,               //OCIStmt      *stmtp,
		(**C.OCIBind)(&bnd.ocibnd),     //OCIBind      **bindpp,
		bnd.stmt.ses.srv.env.ocierr,    //OCIError     *errhp,
		C.ub4(position),                //ub4          position,
		unsafe.Pointer(&bnd.ociNumber), //void         *valuep,
		C.sb8(C.sizeof_OCINumber),      //sb8          value_sz,
		C.SQLT_VNU,                     //ub2          dty,
		nil,                            //void         *indp,
		nil,                            //ub2          *alenp,
		nil,                            //ub2          *rcodep,
		0,                              //ub4          maxarr_len,
		nil,                            //ub4          *curelep,
		C.OCI_DEFAULT)                  //ub4          mode );
	if r == C.OCI_ERROR {
		return bnd.stmt.ses.srv.env.ociError()
	}
	return nil
}

func (bnd *bndUint32) setPtr() error {
	return nil
}

func (bnd *bndUint32) close() (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = errRecover(value)
		}
	}()

	stmt := bnd.stmt
	bnd.stmt = nil
	bnd.ocibnd = nil
	stmt.putBnd(bndIdxUint32, bnd)
	return nil
}