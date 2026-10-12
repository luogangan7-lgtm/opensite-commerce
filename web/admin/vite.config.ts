/** Configures the isolated admin build; called by Vite; loopback is the safe development default. */
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({ plugins: [react()], server: { host: '127.0.0.1' }, preview: { host: '127.0.0.1' } });
