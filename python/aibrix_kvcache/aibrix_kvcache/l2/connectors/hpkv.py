# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from concurrent.futures import Executor
from dataclasses import dataclass

import torch
from hpkv.hpkv_client import HPKVClient

from ... import envs
from ...common import AsyncBase
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from . import Connector, ConnectorFeature, ConnectorRegisterDescriptor


@dataclass
class HPKVConfig:
    """HPKV config.
    Args:
        remote_addr (str): remote address
        remote_port (int): remote port
        local_addr (str): local address
        local_port (int): local port
        num_queues (int): number of queues, default 0
    """

    remote_addr: str
    remote_port: int
    local_addr: str
    local_port: int
    num_queues: int = 0


@dataclass
class HPKVRegisterDescriptor(ConnectorRegisterDescriptor):
    """HPKV register descriptor."""

    reg_buf: int


@AsyncBase.async_wrap(
    exists="_exists", get="_get", put="_put", delete="_delete"
)
class HPKVConnector(Connector[bytes, torch.Tensor], AsyncBase):
    """HPKV connector."""

    def __init__(
        self,
        config: HPKVConfig,
        key_suffix: str,
        executor: Executor,
    ):
        super().__init__(executor)
        self.config = config
        self.key_suffix = key_suffix
        self.conn: HPKVClient | None = None

    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "HPKVConnector":
        """Create a connector from environment variables."""
        remote_addr = kwargs.get(
            "addr", envs.AIBRIX_KV_CACHE_OL_HPKV_REMOTE_ADDR
        )
        remote_port = kwargs.get(
            "port", envs.AIBRIX_KV_CACHE_OL_HPKV_REMOTE_PORT
        )

        config = HPKVConfig(
            remote_addr=remote_addr,
            remote_port=remote_port,
            local_addr=envs.AIBRIX_KV_CACHE_OL_HPKV_LOCAL_ADDR,
            local_port=envs.AIBRIX_KV_CACHE_OL_HPKV_LOCAL_PORT,
        )
        return cls(config, conn_id, executor)

    @property
    def name(self) -> str:
        return "HPKV"

    @property
    def feature(self) -> ConnectorFeature:
        feature = ConnectorFeature(
            rdma=True,
        )
        return feature

    def __del__(self) -> None:
        self.close()

    def _key(self, key: bytes) -> str:
        return key.hex() + self.key_suffix

    @Status.capture_exception
    def open(self) -> Status:
        """Open a connection."""
        if self.conn is None:
            self.conn = HPKVClient(
                raddr=self.config.remote_addr,
                rport=self.config.remote_port,
                laddr=self.config.local_addr,
                lport=self.config.local_port,
                nqueue=self.config.num_queues,
            )
        return Status.ok()

    @Status.capture_exception
    def close(self) -> Status:
        """Close a connection."""
        if self.conn is not None:
            self.conn.close()
            self.conn = None
        return Status.ok()

    @Status.capture_exception
    def register_mr(
        self, addr: int, length: int
    ) -> Status[ConnectorRegisterDescriptor]:
        assert self.conn is not None
        reg_buf = self.conn.reg_memory(addr, length)
        if reg_buf == 0:
            return Status(StatusCodes.INVALID)
        desc = HPKVRegisterDescriptor(reg_buf)
        return Status.ok(desc)

    @Status.capture_exception
    def deregister_mr(self, desc: ConnectorRegisterDescriptor) -> Status:
        assert self.conn is not None
        assert isinstance(desc, HPKVRegisterDescriptor)
        if desc.reg_buf != 0:
            self.conn.dereg_memory(desc.reg_buf)
        desc.reg_buf = 0
        return Status.ok()

    @Status.capture_exception
    def _exists(self, key: bytes) -> Status:
        """Check if key is in the store."""
        assert self.conn is not None
        if self.conn.test(self._key(key)):
            return Status.ok()
        return Status(StatusCodes.NOT_FOUND)

    @Status.capture_exception
    def _get(self, key: bytes, mr: MemoryRegion) -> Status[torch.Tensor]:
        """Get a value."""
        assert self.conn is not None
        desc = mr.register_descriptor()
        if desc is None:
            return Status(StatusCodes.INVALID)
        sgl = self.conn.SGL(mr.data_ptr(), mr.length, desc)
        if self.conn.get(self._key(key), sgl, mr.length) != 0:
            return Status(StatusCodes.ERROR)
        return Status.ok()

    @Status.capture_exception
    def _put(self, key: bytes, mr: MemoryRegion) -> Status:
        """Put a key value pair"""
        assert self.conn is not None
        desc = mr.register_descriptor()
        if desc is None:
            return Status(StatusCodes.INVALID)
        sgl = self.conn.SGL(mr.data_ptr(), mr.length, desc)
        if self.conn.set(self._key(key), sgl) != 0:
            return Status(StatusCodes.ERROR)
        return Status.ok()

    @Status.capture_exception
    def _delete(self, key: bytes) -> Status:
        """Delete a key."""
        assert self.conn is not None
        self.conn.delete_keys(self._key(key))
        return Status.ok()
