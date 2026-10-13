#!/usr/bin/env python3
"""Quotes contract policy gate: platform contract checker only, no runtime auth claim.

Depends on the current OpenAPI and shared checker. check_all invokes this gate;
explicit trusted-buyer admission and mandatory write headers cannot drift.
"""
import copy
import sys
import unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'scripts'))
import check_contract as gate
BUYER={"getBuyerQuote":("get","/api/public/v1/quotes/{quote_id}"),"acceptQuote":("post","/api/public/v1/quotes/{quote_id}/accept"),"approveQuote":("post","/api/public/v1/quotes/{quote_id}/approve"),"rejectQuote":("post","/api/public/v1/quotes/{quote_id}/reject")}
class QuotesContractTests(unittest.TestCase):
    def fixture(self, name):
        api=gate.load_api(ROOT/'api/openapi.yaml')
        # Mutate one implemented operation while preserving the complete contract.
        # Planned/incomplete synthetic inventories mask the security assertion
        # behind the mandatory operation-status/presence gates.
        op=copy.deepcopy(gate.operation_inventory(api)[name][2])
        method,path=BUYER[name]
        op.update(operationId=name,security=[],**{'x-opensite-access':'Public','x-opensite-status':'implemented','x-opensite-trusted-buyer':'required'})
        op['parameters']=[{'name':'quote_id','in':'path','required':True,'schema':{'type':'string','format':'uuid'}}]
        if method=='get':op.pop('requestBody',None)
        else:op['parameters'] += [{'$ref':'#/components/parameters/IfMatch'},{'$ref':'#/components/parameters/IdempotencyKey'}]
        api['paths'][path]={method:op}
        return api,op
    def test_trusted_buyer_contract_accepted(self):
        for name in BUYER:
            with self.subTest(name=name):gate.validate_contract(self.fixture(name)[0])
    def test_buyer_marker_required(self):
        for name in BUYER:
            for value in (None,False,'optional'):
                with self.subTest(name=name,value=value):
                    api,op=self.fixture(name)
                    if value is None:op.pop('x-opensite-trusted-buyer')
                    else:op['x-opensite-trusted-buyer']=value
                    with self.assertRaisesRegex(ValueError,'trusted buyer'):gate.validate_contract(api)
    def test_buyer_route_and_method_bound(self):
        api,op=self.fixture('acceptQuote');path=BUYER['acceptQuote'][1]
        api['paths']['/api/public/v1/unscoped-quotes/{quote_id}/accept']=api['paths'].pop(path)
        with self.assertRaisesRegex(ValueError,'buyer quote route'):gate.validate_contract(api)
    def test_quote_write_headers_required(self):
        for header in ('If-Match','Idempotency-Key'):
            for optional in (False,True):
                with self.subTest(header=header,optional=optional):
                    api,op=self.fixture('acceptQuote')
                    if optional:
                        param=copy.deepcopy(next(gate.resolve(api,p['$ref']) for p in op['parameters'] if '$ref' in p and gate.resolve(api,p['$ref'])['name']==header));param['required']=False
                        op['parameters']=[p for p in op['parameters'] if '$ref' not in p or gate.resolve(api,p['$ref'])['name'] != header]+[param]
                    else:op['parameters']=[p for p in op['parameters'] if '$ref' not in p or gate.resolve(api,p['$ref'])['name'] != header]
                    with self.assertRaisesRegex(ValueError,'quote write'):gate.validate_contract(api)
    def test_seller_route_and_method_bound(self):
        api=gate.load_api(ROOT/'api/openapi.yaml')
        path,method,op=gate.operation_inventory(api)['sendQuote']
        del api['paths'][path][method]
        api['paths'][path]['get']=op
        op['x-required-scope']='quotes:read'
        op.pop('requestBody',None)
        with self.assertRaisesRegex(ValueError,'seller quote route'):gate.validate_contract(api)
        api=gate.load_api(ROOT/'api/openapi.yaml')
        path,_,_=gate.operation_inventory(api)['sendQuote']
        api['paths'][path.replace('/send','/deliver')]=api['paths'].pop(path)
        with self.assertRaisesRegex(ValueError,'seller quote route'):gate.validate_contract(api)
if __name__=='__main__':unittest.main()
